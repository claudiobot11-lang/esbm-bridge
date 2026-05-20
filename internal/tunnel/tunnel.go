// Package tunnel wraps Tailscale's `tsnet` library so the Bridge can
// dial the cloud eRetail server through the ESBM private Tailnet
// without installing the Tailscale system service. Each Bridge is its
// own Tailnet node — appears in the admin console as
// "bridge-<shop_code>" alongside the cloud VPS.
//
// Resilience (2026-05-20): the tunnel is now a SUPERVISED, SELF-HEALING
// worker. Earlier it was started once and never watched, so when tsnet
// drifted into a wedged / one-way state (seen in the admin console as
// "rx 0" — transmitting but receiving nothing) the bridge stayed up but
// the gateway→cloud path was dead, and nothing reconnected (the process
// didn't crash, so the Windows-service recovery never kicked in either).
// Now Run() health-checks the tunnel on a loop and, on sustained
// failure, returns an error so the supervisor rebuilds tsnet from
// scratch — a real auto-reconnect.
package tunnel

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"tailscale.com/tsnet"
)

// Tunnel owns a tsnet.Server instance. The pointer handed to the
// proxies stays stable across reconnects — DialContext reads the
// current srv under a lock, so a restart swaps the underlying node
// without the proxies needing to know.
type Tunnel struct {
	Hostname string
	AuthKey  string
	StateDir string
	Logger   *slog.Logger
	// HealthAddr is dialed THROUGH the tunnel on a loop to prove the
	// VPN can actually reach the cloud (e.g. "100.108.175.3:9071", the
	// eRetail Cronus port). Empty disables active probing.
	HealthAddr string

	mu  sync.RWMutex
	srv *tsnet.Server
}

// start (re)creates the tsnet server and blocks until it's online.
// Any previous instance is closed first so a reconnect doesn't leak a
// half-dead node. tsnet reuses the node identity stored in StateDir, so
// re-Up after the first pairing doesn't need the auth key to be valid
// again.
func (t *Tunnel) start(ctx context.Context) error {
	t.mu.Lock()
	if t.srv != nil {
		_ = t.srv.Close()
		t.srv = nil
	}
	srv := &tsnet.Server{
		Hostname: t.Hostname,
		AuthKey:  t.AuthKey,
		Dir:      t.StateDir,
		// Silence tsnet's stdout chatter; pipe structured logs via slog.
		Logf: func(format string, args ...any) {
			if t.Logger != nil {
				t.Logger.Debug(fmt.Sprintf(format, args...))
			}
		},
	}
	t.srv = srv
	t.mu.Unlock()

	// Up blocks until the node is "online" per Tailscale's control
	// plane — confirms auth + DERP relays are reachable.
	if _, err := srv.Up(ctx); err != nil {
		return fmt.Errorf("tsnet up: %w", err)
	}
	if t.Logger != nil {
		t.Logger.Info("tailscale up", "hostname", t.Hostname)
	}
	return nil
}

// Run is the supervised worker: bring the tunnel up, then health-check
// it forever. Returns nil only on ctx cancel (graceful). Returns an
// error on sustained probe failure so the supervisor restarts us with
// a fresh tsnet — this is the auto-reconnect.
//
// Wire it with:  supervisor.Run(ctx, log, "tunnel", t.Run)
func (t *Tunnel) Run(ctx context.Context) error {
	if err := t.start(ctx); err != nil {
		return err
	}

	const (
		checkEvery = 30 * time.Second
		dialTO     = 8 * time.Second
		failLimit  = 3 // consecutive probe failures → reconnect
	)
	// No probe target → trust tsnet's own DERP self-healing and just
	// block until shutdown.
	if t.HealthAddr == "" {
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c, cancel := context.WithTimeout(ctx, dialTO)
			conn, err := t.DialContext(c, "tcp", t.HealthAddr)
			cancel()
			if err != nil {
				fails++
				if t.Logger != nil {
					t.Logger.Warn("tunnel health check failed",
						"addr", t.HealthAddr, "err", err, "fails", fails)
				}
				if fails >= failLimit {
					// Surface as an error → supervisor tears us down and
					// calls Run again → start() rebuilds tsnet → reconnect.
					return fmt.Errorf("tunnel unhealthy: %d consecutive probe failures to %s",
						fails, t.HealthAddr)
				}
			} else {
				_ = conn.Close()
				if fails > 0 && t.Logger != nil {
					t.Logger.Info("tunnel health recovered", "addr", t.HealthAddr)
				}
				fails = 0
			}
		}
	}
}

// Start brings the tunnel up once (no health loop). Kept for callers
// that manage their own lifecycle; new code should use Run under the
// supervisor instead.
func (t *Tunnel) Start(ctx context.Context) error {
	return t.start(ctx)
}

// DialContext implements proxy.Dialer using the Tailnet. Reads the
// current tsnet server under a read lock so it's safe to call while a
// reconnect swaps the node underneath.
func (t *Tunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	t.mu.RLock()
	srv := t.srv
	t.mu.RUnlock()
	if srv == nil {
		return nil, fmt.Errorf("tunnel not started")
	}
	return srv.Dial(ctx, network, address)
}

// Ready reports whether the tunnel currently has a live tsnet server.
// Used by the dashboard/status pump.
func (t *Tunnel) Ready() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.srv != nil
}

// Close cleanly shuts down the tsnet node. Important before process
// exit so the node isn't left hanging in the admin console.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.srv == nil {
		return nil
	}
	err := t.srv.Close()
	t.srv = nil
	return err
}
