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

	// HealthAddrFunc, when set, supplies the probe target on EVERY probe
	// and wins over HealthAddr. The cloud's tailnet address is not a
	// constant: on 2026-11-13 the eRetail node re-registered with a new
	// 100.x IP and a Bridge that had read the old one at startup probed a
	// dead address for 20 hours. A func lets the caller swap the address
	// under a running tunnel.
	HealthAddrFunc func() string

	// OnUnhealthy runs after every failed probe, before the tunnel
	// decides whether to give up. The caller uses it to re-ask the cloud
	// where eRetail lives, so a moved address is picked up within one
	// probe interval instead of never.
	OnUnhealthy func(context.Context)

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

	// probeFails counts consecutive failed health probes; probedOK is
	// set once a probe has actually reached the cloud since the last
	// bringUp. Together they separate "tsnet says online" from "we can
	// reach eRetail", which Ready() alone never could.
	probeFails int
	probedOK   bool
}

// healthAddr resolves the current probe target.
func (t *Tunnel) healthAddr() string {
	if t.HealthAddrFunc != nil {
		return t.HealthAddrFunc()
	}
	return t.HealthAddr
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
		closeServer(t.srv)
		t.srv = nil
	}
	t.up = false
	t.probeFails = 0
	t.probedOK = false
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
	if key == "" {
		return fmt.Errorf("%w (cloud has no key to offer)", err)
	}
	if key != t.AuthKey {
		t.AuthKey = key
		if t.OnNewKey != nil {
			t.OnNewKey(key)
		}
		err2 := t.bringUp(ctx, key)
		if err2 == nil || !isAuthFailure(err2) {
			return err2
		}
	}

	// Rejected with the key the cloud considers current — either a fresh
	// one that still failed, or the SAME one we already had. The second
	// case used to return "cloud has no newer key" and retry forever;
	// but when the key is good and auth still fails, what is stale is
	// the node identity cached in StateDir: exactly what happens when an
	// EPHEMERAL node is deleted after the bridge has been offline for a
	// while. Wipe it so tsnet registers as a new node. If the key really
	// is dead this changes nothing — there was no working identity left
	// to lose.
	if t.StateDir != "" {
		if t.Logger != nil {
			t.Logger.Warn("resetting tailscale state for clean re-registration", "dir", t.StateDir)
		}
		t.mu.Lock()
		if t.srv != nil {
			closeServer(t.srv) // release the state files before deleting them
			t.srv = nil
		}
		t.up = false
		t.mu.Unlock()
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
	if t.healthAddr() == "" {
		<-ctx.Done()
		return nil
	}

	// probe dials the cloud once through the tunnel. It returns an error
	// only when the failure streak hits failLimit — that error is what
	// makes the supervisor tear tsnet down and rebuild it.
	probe := func() error {
		addr := t.healthAddr()
		c, cancel := context.WithTimeout(ctx, dialTO)
		conn, err := t.DialContext(c, "tcp", addr)
		cancel()
		if ctx.Err() != nil {
			return nil // shutting down — not a health signal
		}
		if err != nil {
			t.mu.Lock()
			t.probeFails++
			fails := t.probeFails
			t.mu.Unlock()
			if t.Logger != nil {
				t.Logger.Warn("tunnel health check failed",
					"addr", addr, "err", err, "fails", fails)
			}
			if t.OnUnhealthy != nil {
				t.OnUnhealthy(ctx)
			}
			if fails >= failLimit {
				// Surface as an error → supervisor tears us down and
				// calls Run again → start() rebuilds tsnet → reconnect.
				return fmt.Errorf("tunnel unhealthy: %d consecutive probe failures to %s",
					fails, addr)
			}
			return nil
		}
		_ = conn.Close()
		t.mu.Lock()
		had := t.probeFails
		t.probeFails = 0
		t.probedOK = true
		t.mu.Unlock()
		if had > 0 && t.Logger != nil {
			t.Logger.Info("tunnel health recovered", "addr", addr)
		}
		return nil
	}

	// Probe once right away instead of waiting a full interval: "tsnet
	// is up" and "eRetail is reachable" are different facts, and the
	// installer and the dashboard need the second one within seconds.
	if err := probe(); err != nil {
		return err
	}

	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := probe(); err != nil {
				return err
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
// constructed. It says tsnet is online; it does NOT say the cloud is
// reachable through it (see Healthy and Reachable).
func (t *Tunnel) Ready() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.srv != nil && t.up
}

// Healthy is what the cloud heartbeat should report: tsnet is up AND we
// are not in a run of failed probes. A single failed probe is tolerated
// so one dropped packet does not page anyone; two in a row (a minute)
// is an outage. Before this, the heartbeat said "tailscale ok" for the
// whole of a 20-hour outage because tsnet itself was perfectly online
// — it was just dialing an address that no longer existed.
func (t *Tunnel) Healthy() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.srv == nil || !t.up {
		return false
	}
	if t.HealthAddrFunc == nil && t.HealthAddr == "" {
		return true // probing disabled: same rule as Reachable()
	}
	// probedOK matters: every rebuild resets the failure count, so
	// without it a tunnel that is up but cannot reach eRetail reads as
	// healthy again for ~45s after each rebuild — and more than half the
	// heartbeats of a sustained outage would say "ok".
	return t.probedOK && t.probeFails < 2
}

// Reachable is the strict, proven form: at least one probe has reached
// the cloud since the tunnel last came up, and none is failing now.
// The installer waits on this before telling the operator it worked.
// With probing disabled it degrades to Ready().
func (t *Tunnel) Reachable() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.srv == nil || !t.up {
		return false
	}
	if t.HealthAddrFunc == nil && t.HealthAddr == "" {
		return true
	}
	return t.probedOK && t.probeFails == 0
}

// Close cleanly shuts down the tsnet node. Important before process
// exit so the node isn't left hanging in the admin console.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.up = false
	closeServer(t.srv)
	t.srv = nil
	return nil
}

// closeServer closes a tsnet server without ever panicking. tsnet's
// Close dereferences internal state that is nil when Start failed early
// (e.g. the state dir could not be created) or never ran. Every caller
// holds t.mu: a panic there is recovered by the supervisor with the
// mutex still locked, which wedges every later dial, probe and
// heartbeat while the process — and the Windows service — look alive.
func closeServer(s *tsnet.Server) {
	if s == nil {
		return
	}
	defer func() { _ = recover() }()
	_ = s.Close()
}
