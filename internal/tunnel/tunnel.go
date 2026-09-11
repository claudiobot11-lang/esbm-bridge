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
	"os"
	"strings"
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

	// KeyFunc fetches a FRESH Tailscale auth key from the cloud. Called
	// only when Up() is rejected for auth — see start(). Without it a
	// dead key wedges the tunnel forever: on 2026-09-09 an expired
	// ephemeral key took the store's 647 tags offline for two days while
	// the supervisor retried the same dead key 1538 times.
	KeyFunc func(context.Context) (string, error)

	// OnNewKey is called after KeyFunc yields a working key so the caller
	// can persist it (config.json) and survive a restart.
	OnNewKey func(string)

	mu  sync.RWMutex
	srv *tsnet.Server
	up  bool // true only AFTER Up() succeeded — see Ready()
}

// isAuthFailure reports whether a tsnet error means our auth key was
// rejected (expired, revoked, or deleted from the tailnet) rather than a
// transient network problem. Observed string: "tsnet up: tsnet:Up:
// backend: invalid key: API key does not exist".
func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, needle := range []string{
		"invalid key", "api key does not exist", "unauthorized",
		"invalid auth", "key expired", "expired", "needs login", "logged out",
	} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// start (re)creates the tsnet server and blocks until it's online.
// Any previous instance is closed first so a reconnect doesn't leak a
// half-dead node. tsnet reuses the node identity stored in StateDir, so
// re-Up after the first pairing doesn't need the auth key to be valid
// again.
// bringUp (re)creates the tsnet server with the given key and blocks
// until it's online. `up` is only set true AFTER Up() returns — the old
// code marked the tunnel ready the moment the struct existed, so the
// dashboard and the cloud heartbeat both reported a healthy tunnel while
// it was in fact dead.
func (t *Tunnel) bringUp(ctx context.Context, key string) error {
	t.mu.Lock()
	if t.srv != nil {
		_ = t.srv.Close()
		t.srv = nil
	}
	t.up = false
	srv := &tsnet.Server{
		Hostname: t.Hostname,
		AuthKey:  key,
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
	t.mu.Lock()
	t.up = true
	t.mu.Unlock()
	if t.Logger != nil {
		t.Logger.Info("tailscale up", "hostname", t.Hostname)
	}
	return nil
}

// start brings the tunnel up, and — this is the 2026-09 fix — recovers
// when the auth key itself is dead instead of retrying it forever.
// Escalation: fresh key from the cloud → if still rejected, wipe the
// stale node identity and register clean.
func (t *Tunnel) start(ctx context.Context) error {
	err := t.bringUp(ctx, t.AuthKey)
	if err == nil || !isAuthFailure(err) || t.KeyFunc == nil {
		return err
	}

	if t.Logger != nil {
		t.Logger.Warn("tailscale auth rejected — fetching a fresh key", "err", err)
	}
	key, kerr := t.KeyFunc(ctx)
	if kerr != nil {
		return fmt.Errorf("%w (fresh key fetch failed: %v)", err, kerr)
	}
	if key == "" || key == t.AuthKey {
		return fmt.Errorf("%w (cloud has no newer key)", err)
	}
	t.AuthKey = key
	if t.OnNewKey != nil {
		t.OnNewKey(key)
	}

	err2 := t.bringUp(ctx, key)
	if err2 == nil || !isAuthFailure(err2) {
		return err2
	}

	// Still rejected: the node identity cached in StateDir is stale —
	// exactly what happens when an EPHEMERAL node gets deleted after the
	// bridge goes offline. Wipe it so tsnet registers as a new node.
	if t.StateDir != "" {
		if t.Logger != nil {
			t.Logger.Warn("resetting tailscale state for clean re-registration", "dir", t.StateDir)
		}
		_ = os.RemoveAll(t.StateDir)
	}
	return t.bringUp(ctx, key)
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

// Ready reports whether the tunnel is actually UP — not merely
// constructed. Both the dashboard and the cloud heartbeat read this, so
// it must never claim health the tunnel doesn't have.
func (t *Tunnel) Ready() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.srv != nil && t.up
}

// Close cleanly shuts down the tsnet node. Important before process
// exit so the node isn't left hanging in the admin console.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.up = false
	if t.srv == nil {
		return nil
	}
	err := t.srv.Close()
	t.srv = nil
	return err
}
