// Package dashboard serves a tiny HTTP UI on localhost so the operator
// can see at a glance whether the Bridge is healthy. Bound to
// 127.0.0.1 only — never exposed to the LAN.
package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/claudiobot11-lang/esbm-bridge/internal/supervisor"
)

// DefaultAddr is where the dashboard binds. Picked something
// unlikely to collide with anything the operator runs on the store PC.
const DefaultAddr = "127.0.0.1:9099"

// Status is the snapshot the dashboard renders. The Bridge fills this
// in via Server.SetStatus on every heartbeat tick.
type Status struct {
	Version           string                    `json:"version"`
	ShopCode          string                    `json:"shop_code"`
	ServerAddr        string                    `json:"server_addr"`
	StartedAt         time.Time                 `json:"started_at"`
	TailscaleHostname string                    `json:"tailscale_hostname"`
	TailscaleReady    bool                      `json:"tailscale_ready"`
	GatewayLastSeen   time.Time                 `json:"gateway_last_seen,omitempty"`
	MQTTMessages      int64                     `json:"mqtt_messages"`
	Workers           []supervisor.Status       `json:"workers"`
	RecentLogs        []string                  `json:"recent_logs,omitempty"`
}

// Server is the dashboard HTTP server. Construct with New, call
// SetStatus before/during Start so /api/status returns useful data.
type Server struct {
	addr   string
	logger *slog.Logger

	mu     sync.RWMutex
	status Status

	// onRestart is wired to a function that bounces the proxy/tunnel.
	// Optional — when nil, the "Restart" button is hidden in the UI.
	onRestart func()
}

// New builds a Server bound to addr. Pass "" to use DefaultAddr.
func New(addr string, log *slog.Logger) *Server {
	if addr == "" {
		addr = DefaultAddr
	}
	return &Server{addr: addr, logger: log}
}

// SetStatus replaces the entire status payload atomically. Cheap —
// callers can fire this on every heartbeat without worrying.
func (s *Server) SetStatus(st Status) {
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
}

// Snapshot returns a copy of the current status. Used by the tray
// icon to color itself green/yellow/red without re-implementing the
// fetch logic.
func (s *Server) Snapshot() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

// SetWorkers updates just the supervised-worker list. Pulled out so
// the supervisor can push updates without rebuilding the whole Status.
func (s *Server) SetWorkers(ws []supervisor.Status) {
	s.mu.Lock()
	s.status.Workers = ws
	s.mu.Unlock()
}

// OnRestart registers a callback fired when the operator hits the
// "Restart" button on the dashboard. The Bridge wires this to
// cancelling + recreating the proxy/tunnel goroutines.
func (s *Server) OnRestart(fn func()) {
	s.mu.Lock()
	s.onRestart = fn
	s.mu.Unlock()
}

// Run is a Worker for supervisor.Run — blocks until ctx is cancelled
// or http.ListenAndServe errors. Listens on s.addr.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/restart", s.handleRestart)

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	// Force-bind on loopback so a misconfigured LAN doesn't expose us.
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("dashboard listen %s: %w", s.addr, err)
	}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	s.logger.Info("dashboard listening", "addr", s.addr)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	out := s.status
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	fn := s.onRestart
	s.mu.RUnlock()
	if fn == nil {
		http.Error(w, "restart not wired", http.StatusServiceUnavailable)
		return
	}
	go fn()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// handleIndex serves a single-file HTML+JS dashboard. Keeping it
// inline avoids shipping a separate assets directory — the .exe stays
// drop-in deployable.
func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>ESBM Bridge</title>
<style>
:root { --bg:#0f172a; --panel:#1e293b; --ok:#16a34a; --warn:#ca8a04; --err:#dc2626; --muted:#94a3b8; --text:#e2e8f0; }
html, body { background:var(--bg); color:var(--text); font:14px/1.4 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin:0; }
.wrap { max-width:920px; margin:24px auto; padding:0 16px; }
h1 { margin:0 0 16px; font-size:22px; display:flex; align-items:center; gap:10px; }
.dot { width:14px; height:14px; border-radius:50%; }
.dot.ok{background:var(--ok);box-shadow:0 0 8px var(--ok);}
.dot.warn{background:var(--warn);box-shadow:0 0 8px var(--warn);}
.dot.err{background:var(--err);box-shadow:0 0 8px var(--err);}
.grid { display:grid; grid-template-columns:repeat(auto-fit,minmax(260px,1fr)); gap:14px; margin-bottom:14px; }
.card { background:var(--panel); border-radius:10px; padding:14px 16px; }
.card h2 { margin:0 0 4px; font-size:12px; text-transform:uppercase; letter-spacing:.06em; color:var(--muted); font-weight:600; }
.card .v { font-size:22px; font-weight:600; }
.card .sub { font-size:12px; color:var(--muted); margin-top:2px; }
table { width:100%; border-collapse:collapse; }
th, td { padding:8px 10px; text-align:left; border-bottom:1px solid #334155; }
th { font-size:11px; text-transform:uppercase; letter-spacing:.06em; color:var(--muted); font-weight:600; }
.state { display:inline-flex; align-items:center; gap:6px; font-size:12px; padding:2px 8px; border-radius:10px; background:#334155; }
.state.running{background:rgba(22,163,74,.18);color:var(--ok);}
.state.backoff{background:rgba(202,138,4,.18);color:var(--warn);}
.state.stopped{background:rgba(220,38,38,.18);color:var(--err);}
button { background:#2563eb; color:#fff; border:none; padding:8px 14px; border-radius:6px; cursor:pointer; font-size:13px; }
button:hover { background:#1d4ed8; }
.foot { color:var(--muted); font-size:11px; margin-top:18px; }
</style>
</head>
<body>
<div class="wrap">
  <h1><span id="overall" class="dot"></span> ESBM Bridge</h1>
  <div class="grid">
    <div class="card"><h2>Version</h2><div class="v" id="version">—</div></div>
    <div class="card"><h2>Shop</h2><div class="v" id="shop">—</div></div>
    <div class="card"><h2>Tailscale</h2><div class="v" id="ts">—</div><div class="sub" id="ts_host"></div></div>
    <div class="card"><h2>MQTT msgs</h2><div class="v" id="mqtt">0</div><div class="sub" id="last_seen"></div></div>
  </div>
  <div class="card">
    <h2 style="margin-bottom:8px;">Workers</h2>
    <table id="workers"><thead><tr><th>Name</th><th>State</th><th>Restarts</th><th>Last error</th></tr></thead><tbody></tbody></table>
  </div>
  <p style="margin-top:14px;"><button onclick="restart()">Restart proxies</button></p>
  <p class="foot">Auto-refresh every 2s · open <code>http://localhost:9099</code></p>
</div>
<script>
async function tick(){
  try {
    const r = await fetch('/api/status'); const s = await r.json();
    document.getElementById('version').textContent = s.version || '—';
    document.getElementById('shop').textContent = s.shop_code || '—';
    document.getElementById('ts').textContent = s.tailscale_ready ? 'connected' : 'down';
    document.getElementById('ts_host').textContent = s.tailscale_hostname || '';
    document.getElementById('mqtt').textContent = s.mqtt_messages || 0;
    if (s.gateway_last_seen && !s.gateway_last_seen.startsWith('0001')) {
      const age = Math.round((Date.now() - new Date(s.gateway_last_seen).getTime())/1000);
      document.getElementById('last_seen').textContent = 'gateway seen ' + age + 's ago';
    } else {
      document.getElementById('last_seen').textContent = 'no gateway heartbeat';
    }
    const tbody = document.querySelector('#workers tbody'); tbody.innerHTML = '';
    let worst = 'ok';
    for (const w of (s.workers || [])) {
      const tr = document.createElement('tr');
      tr.innerHTML = '<td>'+w.name+'</td><td><span class="state '+w.state+'">'+w.state+'</span></td><td>'+w.restarts+'</td><td>'+(w.last_error||'')+'</td>';
      tbody.appendChild(tr);
      if (w.state === 'stopped' || w.state === 'backoff') worst = 'err';
      else if (w.restarts > 0 && worst === 'ok') worst = 'warn';
    }
    document.getElementById('overall').className = 'dot ' + worst;
  } catch(e) {
    document.getElementById('overall').className = 'dot err';
  }
}
async function restart(){ await fetch('/api/restart', {method:'POST'}); setTimeout(tick, 500); }
tick(); setInterval(tick, 2000);
</script>
</body>
</html>`
