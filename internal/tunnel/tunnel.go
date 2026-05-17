// Package tunnel wraps Tailscale's `tsnet` library so the Bridge can
// dial the cloud eRetail server through the ESBM private Tailnet
// without installing the Tailscale system service. Each Bridge is its
// own Tailnet node — appears in the admin console as
// "bridge-<shop_code>" alongside the cloud VPS.
//
// The first Run() consumes the auth key from Config; subsequent
// starts reuse the node identity stored in tsnet's state dir.
package tunnel

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"tailscale.com/tsnet"
)

// Tunnel owns a tsnet.Server instance. Lifetime matches the Bridge
// process — start once at boot, Close on shutdown.
type Tunnel struct {
	Hostname string
	AuthKey  string
	StateDir string
	Logger   *slog.Logger

	srv *tsnet.Server
}

// Start brings the Tailnet node up and blocks until it's online.
// Returns an error if auth or networking fails — caller surfaces it
// to esbm-app as a critical event before retrying.
func (t *Tunnel) Start(ctx context.Context) error {
	t.srv = &tsnet.Server{
		Hostname: t.Hostname,
		AuthKey:  t.AuthKey,
		Dir:      t.StateDir,
		// Silence tsnet's stdout chatter in production; pipe
		// structured logs via our slog instead.
		Logf: func(format string, args ...any) {
			if t.Logger != nil {
				t.Logger.Debug(fmt.Sprintf(format, args...))
			}
		},
	}
	// Up blocks until the node is "online" per Tailscale's control
	// plane — confirms auth key worked and DERP relays are reachable.
	if _, err := t.srv.Up(ctx); err != nil {
		return fmt.Errorf("tsnet up: %w", err)
	}
	if t.Logger != nil {
		t.Logger.Info("tailscale up", "hostname", t.Hostname)
	}
	return nil
}

// DialContext implements proxy.Dialer using the Tailnet. Every TCP
// connection made through this function travels inside the VPN.
func (t *Tunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if t.srv == nil {
		return nil, fmt.Errorf("tunnel not started")
	}
	return t.srv.Dial(ctx, network, address)
}

// Close cleanly shuts down the tsnet node. Important to call before
// process exit so the node isn't left hanging in the admin console.
func (t *Tunnel) Close() error {
	if t.srv == nil {
		return nil
	}
	return t.srv.Close()
}
