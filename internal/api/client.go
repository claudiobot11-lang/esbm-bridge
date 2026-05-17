// Package api is the HTTP client to esbm-app. The Bridge calls
// esbm-app for three things: (1) one-time pairing using the 6-digit
// code shown in /esl/stores, (2) periodic heartbeats reporting status,
// and (3) polling for admin-issued commands (restart, update, diag).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a stateless wrapper around http.Client. Each call carries
// the Bridge JWT in Authorization, except Pair which uses the one-shot
// code in the body.
type Client struct {
	BaseURL string
	JWT     string
	HTTP    *http.Client
}

// New builds a Client with a sensible HTTP timeout. The Bridge talks
// to Railway over the public internet — long-haul, so 30s is generous
// but bounded.
func New(baseURL, jwt string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		JWT:     jwt,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// PairResponse is the payload esbm-app returns after validating the
// 6-digit code. Everything the Bridge needs to operate end-to-end
// fits in here so we don't need follow-up fetches at boot.
type PairResponse struct {
	JWT              string `json:"jwt"`
	TailscaleAuthKey string `json:"ts_auth_key"`
	ShopCode         string `json:"shop_code"`
	ServerAddr       string `json:"server_addr"`
}

// Pair exchanges the 6-digit pairing code for a long-lived Bridge JWT
// + Tailscale auth key. Should be called exactly once per install.
// Codes are short-lived (10 min) on the esbm-app side, so wall-clock
// drift between PC and cloud matters.
func (c *Client) Pair(ctx context.Context, code string) (*PairResponse, error) {
	body, _ := json.Marshal(map[string]string{"code": code})
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+"/api/esl/bridge/pair", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("pair: HTTP %d: %s", resp.StatusCode, truncate(raw, 200))
	}
	var pr PairResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		return nil, fmt.Errorf("parse pair response: %w", err)
	}
	if pr.JWT == "" {
		return nil, fmt.Errorf("pair response missing jwt: %s", truncate(raw, 200))
	}
	return &pr, nil
}

// Heartbeat is the periodic status ping. Includes coarse counters so
// the admin dashboard can show "live" data without per-event logging.
type Heartbeat struct {
	UptimeSec      int64  `json:"uptime_sec"`
	Version        string `json:"version"`
	GatewayCount   int    `json:"gateway_count"`
	MQTTMsgsLastMin int64 `json:"mqtt_msgs_last_min"`
	TailscaleOK    bool   `json:"tailscale_ok"`
}

// SendHeartbeat posts one heartbeat. Errors are non-fatal — the caller
// loops on a timer and tolerates intermittent failures.
func (c *Client) SendHeartbeat(ctx context.Context, hb Heartbeat) error {
	return c.postJSON(ctx, "/api/esl/bridge/heartbeat", hb, nil)
}

// Event reports a one-off occurrence to esbm-app (gateway connected,
// MQTT auth failure, etc.). Severity drives badging on the dashboard.
type Event struct {
	Type     string         `json:"type"`
	Severity string         `json:"severity"` // info | warn | error
	Message  string         `json:"message"`
	Context  map[string]any `json:"context,omitempty"`
}

// SendEvent fires one event. Like heartbeats, non-fatal on failure.
func (c *Client) SendEvent(ctx context.Context, ev Event) error {
	return c.postJSON(ctx, "/api/esl/bridge/event", ev, nil)
}

// PendingCommand is the polled-action shape. Bridge polls every
// ~10s and runs whatever is queued (restart, diagnostic bundle, etc.).
type PendingCommand struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

// PollCommands fetches pending admin commands. Empty slice if none.
func (c *Client) PollCommands(ctx context.Context) ([]PendingCommand, error) {
	var out struct {
		Commands []PendingCommand `json:"commands"`
	}
	if err := c.getJSON(ctx, "/api/esl/bridge/commands", &out); err != nil {
		return nil, err
	}
	return out.Commands, nil
}

// AckCommand marks an admin command as completed so esbm-app removes
// it from the queue. Call this *after* the action actually finished.
func (c *Client) AckCommand(ctx context.Context, id string, result string) error {
	return c.postJSON(ctx, "/api/esl/bridge/commands/"+id+"/ack",
		map[string]string{"result": result}, nil)
}

func (c *Client) postJSON(ctx context.Context, path string, body any, out any) error {
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.JWT)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, truncate(raw, 200))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.JWT)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, truncate(raw, 200))
	}
	return json.Unmarshal(raw, out)
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
