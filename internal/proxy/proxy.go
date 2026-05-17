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

	// Dialer is how we open the upstream conn. tsnet in prod.
	Dialer Dialer

	// Logger receives structured connection events. Nil = discard.
	Logger *slog.Logger

	// msgs counts bytes in either direction in the last 60s window.
	// Heartbeat reports it; resets on each rotation tick.
	msgs atomic.Int64
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

	s.log("proxy listening", "addr", s.Listen, "upstream", s.Upstream)

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
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	out, err := s.Dialer.DialContext(dialCtx, "tcp", s.Upstream)
	cancel()
	if err != nil {
		s.log("upstream dial failed", "remote", remote, "err", err.Error())
		return
	}
	defer out.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go s.copy(&wg, out, in, "gateway→cloud")
	go s.copy(&wg, in, out, "cloud→gateway")
	wg.Wait()

	s.log("gateway disconnected", "remote", remote)
}

func (s *Server) copy(wg *sync.WaitGroup, dst io.Writer, src io.Reader, dir string) {
	defer wg.Done()
	// io.Copy returns when either side closes — the deferred Close
	// in handle() then unblocks the other goroutine.
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
