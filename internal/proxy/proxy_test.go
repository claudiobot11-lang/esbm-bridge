package proxy

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// upstream is a tiny TCP server that records when each accepted
// connection sees EOF, i.e. when the proxy let go of it.
type upstream struct {
	ln     net.Listener
	conns  atomic.Int64
	closed atomic.Int64
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &upstream{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u.conns.Add(1)
			go func() {
				_, _ = io.Copy(io.Discard, c) // returns when the proxy closes its side
				u.closed.Add(1)
				c.Close()
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return u
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startProxy runs s on a free loopback port and returns its address.
func startProxy(t *testing.T, s *Server) string {
	t.Helper()
	// Grab a free port, release it, and let the proxy bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	s.Listen = addr
	s.Dialer = &net.Dialer{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Run(ctx) }()
	return addr
}

// dialProxy connects to the proxy, retrying until it is listening. The
// first successful dial is the only connection it makes, so tests can
// count upstream sessions exactly.
func dialProxy(t *testing.T, addr string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy never started listening on %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// When the gateway hangs up, the proxy must drop the upstream session
// too. Before the fix it stayed open forever: one leaked goroutine and
// one live cloud session per gateway reconnect.
func TestGatewayDisconnectClosesUpstream(t *testing.T) {
	u := newUpstream(t)
	addr := startProxy(t, &Server{Upstream: u.ln.Addr().String()})

	gw := dialProxy(t, addr)
	if _, err := gw.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "upstream session to open", func() bool { return u.conns.Load() == 1 })
	if u.closed.Load() != 0 {
		t.Fatal("upstream session closed while the gateway was still connected")
	}
	gw.Close() // the gateway goes away; the cloud side has nothing to say

	waitFor(t, "upstream to be released after the gateway hung up",
		func() bool { return u.closed.Load() == 1 })
}

// UpstreamFunc is consulted per connection, so an address learned while
// the Bridge is running takes effect on the gateway's next reconnect.
func TestUpstreamFuncIsResolvedPerConnection(t *testing.T) {
	a, b := newUpstream(t), newUpstream(t)
	var cur atomic.Pointer[string]
	set := func(u *upstream) { s := u.ln.Addr().String(); cur.Store(&s) }
	set(a)
	addr := startProxy(t, &Server{
		Upstream:     "127.0.0.1:1", // must be ignored when UpstreamFunc is set
		UpstreamFunc: func() string { return *cur.Load() },
	})

	first := dialProxy(t, addr)
	defer first.Close()
	waitFor(t, "first connection to land on A", func() bool { return a.conns.Load() == 1 })
	if b.conns.Load() != 0 {
		t.Fatal("B got a connection before the address changed")
	}

	set(b) // the cloud moved
	second := dialProxy(t, addr)
	defer second.Close()
	waitFor(t, "next connection to land on B", func() bool { return b.conns.Load() == 1 })
	if a.conns.Load() != 1 {
		t.Fatal("a connection made after the change still went to A")
	}
}

// When the upstream address changes, sessions still attached to the old
// one must be cut — they would otherwise sit on a dead socket — while a
// session already on the current address is left alone.
func TestDropStaleCutsOnlySessionsOnTheOldUpstream(t *testing.T) {
	a, b := newUpstream(t), newUpstream(t)
	var cur atomic.Pointer[string]
	set := func(u *upstream) { s := u.ln.Addr().String(); cur.Store(&s) }
	set(a)
	srv := &Server{UpstreamFunc: func() string { return *cur.Load() }}
	addr := startProxy(t, srv)

	onA := dialProxy(t, addr)
	defer onA.Close()
	waitFor(t, "session on A", func() bool { return a.conns.Load() == 1 })

	srv.DropStale() // nothing changed yet: must be a no-op
	time.Sleep(50 * time.Millisecond)
	if a.closed.Load() != 0 {
		t.Fatal("DropStale cut a session whose upstream is still current")
	}

	set(b) // the cloud moved
	onB := dialProxy(t, addr)
	defer onB.Close()
	waitFor(t, "session on B", func() bool { return b.conns.Load() == 1 })

	srv.DropStale()
	waitFor(t, "stale session on A to be cut", func() bool { return a.closed.Load() == 1 })

	// The gateway side of the stale session sees the close too…
	_ = onA.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := onA.Read(make([]byte, 1)); err == nil {
		t.Fatal("gateway side of the stale session is still open")
	}
	// …and the current session keeps working.
	time.Sleep(50 * time.Millisecond)
	if b.closed.Load() != 0 {
		t.Fatal("DropStale cut the session on the CURRENT upstream")
	}
	if _, err := onB.Write([]byte("still here")); err != nil {
		t.Fatalf("current session broken: %v", err)
	}
}
