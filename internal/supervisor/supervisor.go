// Package supervisor wraps long-running goroutines with panic recovery
// and exponential-backoff restarts so transient failures don't take
// the whole Bridge process down.
//
// Use it for anything that should run "forever" while the Bridge is
// up — the Tailscale tunnel, each proxy listener, the heartbeat
// loop, the dashboard HTTP server, etc.
package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
	"time"
)

// Status captures the runtime state of a supervised worker. The
// dashboard reads it to render the live health UI.
type Status struct {
	Name        string    `json:"name"`
	State       string    `json:"state"` // "starting" | "running" | "backoff" | "stopped"
	Restarts    int64     `json:"restarts"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
}

// Worker is the function the supervisor runs. Return nil for graceful
// shutdown (no restart). Any non-nil error — or panic — triggers a
// backoff + restart unless the ctx was cancelled.
type Worker func(ctx context.Context) error

// Supervised is the live handle to a running worker. Read .Snapshot()
// to get a status copy without locking.
type Supervised struct {
	name        string
	state       atomic.Pointer[string]
	restarts    atomic.Int64
	lastError   atomic.Pointer[string]
	lastErrorAt atomic.Int64 // unix nano
	startedAt   atomic.Int64 // unix nano
}

// Snapshot returns the worker's current Status. Safe to call from any
// goroutine; the returned value is a copy.
func (s *Supervised) Snapshot() Status {
	st := "starting"
	if p := s.state.Load(); p != nil {
		st = *p
	}
	var lastErr string
	if p := s.lastError.Load(); p != nil {
		lastErr = *p
	}
	var lastAt, started time.Time
	if v := s.lastErrorAt.Load(); v != 0 {
		lastAt = time.Unix(0, v)
	}
	if v := s.startedAt.Load(); v != 0 {
		started = time.Unix(0, v)
	}
	return Status{
		Name:        s.name,
		State:       st,
		Restarts:    s.restarts.Load(),
		LastError:   lastErr,
		LastErrorAt: lastAt,
		StartedAt:   started,
	}
}

func (s *Supervised) setState(v string) {
	s.state.Store(&v)
}

func (s *Supervised) setLastError(err string) {
	s.lastError.Store(&err)
	s.lastErrorAt.Store(time.Now().UnixNano())
}

// Run starts the worker in a goroutine with panic recovery and an
// exponential-backoff restart loop (500ms → 1s → 2s → … → 30s cap,
// reset after 60s of stable run). Returns a Supervised handle whose
// Snapshot is the live status. Blocks the caller for one tick only
// — the actual work happens in the goroutine.
func Run(ctx context.Context, log *slog.Logger, name string, w Worker) *Supervised {
	s := &Supervised{name: name}
	s.setState("starting")

	go func() {
		const (
			minBackoff = 500 * time.Millisecond
			maxBackoff = 30 * time.Second
			stableFor  = 60 * time.Second // reset backoff after this much uptime
		)
		backoff := minBackoff
		for {
			if ctx.Err() != nil {
				s.setState("stopped")
				return
			}
			s.setState("running")
			s.startedAt.Store(time.Now().UnixNano())
			runStart := time.Now()
			err := runOnce(ctx, w)
			ran := time.Since(runStart)

			// Graceful exit: no restart.
			if err == nil && ctx.Err() != nil {
				s.setState("stopped")
				return
			}
			s.restarts.Add(1)
			if err != nil {
				s.setLastError(err.Error())
				log.Warn("supervised worker exited",
					"name", name, "err", err, "ran", ran,
					"restarts", s.restarts.Load())
			} else {
				log.Warn("supervised worker returned nil but ctx is alive — restarting",
					"name", name, "ran", ran)
			}

			// Backoff decision: if we ran for at least stableFor, the
			// previous failure was an isolated event, so reset.
			if ran >= stableFor {
				backoff = minBackoff
			} else {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			s.setState("backoff")
			select {
			case <-ctx.Done():
				s.setState("stopped")
				return
			case <-time.After(backoff):
			}
		}
	}()
	return s
}

// runOnce executes w(ctx) with a deferred recover that converts a
// panic into a returned error. Keeps the Bridge process alive even
// when a third-party library throws.
func runOnce(ctx context.Context, w Worker) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return w(ctx)
}
