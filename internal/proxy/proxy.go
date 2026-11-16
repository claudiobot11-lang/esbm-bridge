// Package proxy is the TCP forwarder that sits between the local
// S-ETAP05 gateway and the cloud eRetail MQTT broker. The gateway
// can only dial an MQTT server on its LAN; we listen on this PC's
// LAN address (port 9071) and stream every byte over the Tailscale
// tunnel to the eRetail container running on Hetzner.
//
// We intentionally don't speak MQTT here — we just shuttle bytes.
// Lets eRetail change protocol details without us needing updates.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Dialer abstracts how we open the *upstream* connection. In
// production this is `tsnet.Server.Dial` so traffic flows over
// Tailscale; tests can swap in net.Dialer for local loopback.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Server is one bidirectional TCP forwarder. One Server handles one
// listening port — the Bridge usually runs a single Server on 9071.
type Server struct {
	// Listen is the LAN address the gateway dials. "0.0.0.0:9071"
	// when the gateway and PC are on the same router.
	Listen string

	// Upstream is the eRetail MQTT endpoint inside the Tailnet —
	// e.g. "100.108.175.3:9071". The Dialer resolves and routes it.
	Upstream string

	// UpstreamFunc, when set, supplies the upstream for each NEW
	// connection and wins over Upstream. The cloud's tailnet address can
	// change under a running Bridge (it did, 2026-11-13); resolving it
	// per connection means the gateway's next reconnect lands on the new
	// address without restarting anything.
	UpstreamFunc func() string

	// Dialer is how we open the upstream conn. tsnet in prod.
	Dialer Dialer

	// Logger receives structured connection events. Nil = discard.
	Logger *slog.Logger

	// msgs counts bytes in either direction in the last 60s window.
	// Heartbeat reports it; resets on each rotation tick.
	msgs atomic.Int64

	// sess tracks live gateway connections and the upstream each one
	// was dialed to, so DropStale can cut the ones left on a dead
	// address.
	sessMu sync.Mutex
	sess   map[net.Conn]string
}

// DropStale closes every session whose upstream is no longer the
// current one. Call it right after the upstream address changes: a
// session attached to the old address gets no FIN or RST when the cloud
// node moves — its packets are simply dropped — so without this the
// gateway sits on a dead socket until its own keepalive gives up, which
// can take many minutes, while every health signal is already green.
// The gateway reconnects on its own and lands on the new address.
func (s *Server) DropStale() {
	cur := s.upstream()
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	for c, up := range s.sess {
		if up != cur {
			_ = c.Close() // unblocks handle(), which tears down both sides
		}
	}
}

// Run blocks accepting connections until ctx is cancelled or Listen
// fails to bind. Returning means the Bridge should report a critical
// error to esbm-app — without proxy, the gateway is useless.
func (s *Server) Run(ctx context.Context) error {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", s.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.Listen, err)
	}
	defer ln.Close()

	s.log("proxy listening", "addr", s.Listen, "upstream", s.upstream())

	// Stop accepting when the context cancels — closing the
	// listener unblocks Accept with a net.ErrClosed.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return ctx.Err()
			}
			s.log("accept error", "err", err.Error())
			continue
		}
		go s.handle(ctx, conn)
	}
}

// MsgsLastMin returns the counter for dashboard reporting and resets
// it. Called once per heartbeat tick (~60s).
func (s *Server) MsgsLastMin() int64 {
	return s.msgs.Swap(0)
}

func (s *Server) handle(ctx context.Context, in net.Conn) {
	defer in.Close()
	remote := in.RemoteAddr().String()
	s.log("gateway connected", "remote", remote)

	// Bound the upstream dial — gateway will retry on its own if we
	// hit a transient Tailscale issue, no need to hold the socket.
	up := s.upstream()
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	out, err := s.Dialer.DialContext(dialCtx, "tcp", up)
	cancel()
	if err != nil {
		s.log("upstream dial failed", "remote", remote, "upstream", up, "err", err.Error())
		return
	}
	defer out.Close()

	s.sessMu.Lock()
	if s.sess == nil {
		s.sess = map[net.Conn]string{}
	}
	s.sess[in] = up
	s.sessMu.Unlock()
	defer func() {
		s.sessMu.Lock()
		delete(s.sess, in)
		s.sessMu.Unlock()
	}()

	// The moment EITHER direction ends, close both sockets. The old code
	// only did wg.Wait(): when the gateway hung up, the cloud→gateway
	// copy stayed blocked reading an upstream that had no reason to
	// close, so every gateway reconnect leaked a goroutine and a live
	// session on the eRetail side — for a process that now runs for
	// months as a service. Same teardown on shutdown (ctx).
	var wg sync.WaitGroup
	wg.Add(2)
	first := make(chan struct{}, 2)
	go func() { s.copy(&wg, out, in, "gateway→cloud"); first <- struct{}{} }()
	go func() { s.copy(&wg, in, out, "cloud→gateway"); first <- struct{}{} }()
	select {
	case <-first:
	case <-ctx.Done():
	}
	_ = in.Close()
	_ = out.Close()
	wg.Wait()

	s.log("gateway disconnected", "remote", remote)
}

// upstream resolves where this connection should go.
func (s *Server) upstream() string {
	if s.UpstreamFunc != nil {
		return s.UpstreamFunc()
	}
	return s.Upstream
}

func (s *Server) copy(wg *sync.WaitGroup, dst io.Writer, src io.Reader, dir string) {
	defer wg.Done()
	// io.Copy returns when its source closes; handle() closes both
	// sockets as soon as the first direction ends, unblocking the other.
	n, err := io.Copy(dst, src)
	s.msgs.Add(n)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		s.log("copy ended with error", "dir", dir, "bytes", n, "err", err.Error())
	}
}

func (s *Server) log(msg string, args ...any) {
	if s.Logger == nil {
		return
	}
	s.Logger.Info(msg, args...)
}
