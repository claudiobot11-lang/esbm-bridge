// Package config handles the Bridge's persisted state — the JWT issued
// by esbm-app at pairing time, the Tailscale auth key for this device,
// and the resolved eRetail server address. The file lives under
// %ProgramData% on Windows so it survives user-profile resets; on
// macOS/Linux we fall back to $XDG_CONFIG_HOME for dev work.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Config is the on-disk Bridge state. Written once at `pair`, read on
// every `run`. Avoid adding mutable fields here — heartbeat counters
// and runtime metrics live in memory.
type Config struct {
	// BridgeJWT is issued by esbm-app /api/esl/bridge/pair. The Bridge
	// includes it as `Authorization: Bearer <jwt>` on every subsequent
	// call (heartbeat, event, config fetch, command poll).
	BridgeJWT string `json:"bridge_jwt"`

	// TailscaleAuthKey is an ephemeral key minted by esbm-app at pair
	// time. tsnet uses it on first start; after that the node identity
	// is stored under TailscaleStateDir and the key isn't needed.
	TailscaleAuthKey string `json:"ts_auth_key"`

	// TailscaleStateDir is where tsnet persists the node identity
	// (machine key, DNS state, etc.). Defaults to <ConfigDir>/tailnet.
	TailscaleStateDir string `json:"ts_state_dir,omitempty"`

	// ShopCode mirrors the eRetail shopCode for this store
	// (e.g. "ESBM-SB"). Logged with every heartbeat so the dashboard
	// can group multi-store data.
	ShopCode string `json:"shop_code"`

	// ServerAddr is the eRetail MQTT endpoint inside the Tailnet
	// (e.g. "100.108.175.3:9071"). esbm-app fills this so the Bridge
	// doesn't need to hard-code the cloud VPS IP.
	ServerAddr string `json:"server_addr"`

	// EsbmAppURL is the public esbm-app base URL used for heartbeat
	// and command-polling. Defaults to production Railway.
	EsbmAppURL string `json:"esbm_app_url"`

	// Hostname overrides the default Tailscale-visible name. Defaults
	// to "bridge-<shop_code>" if empty.
	Hostname string `json:"hostname,omitempty"`
}

// Path returns the absolute path of the config file for the current
// OS. We pick %ProgramData% on Windows so the file survives a user
// profile wipe — the Bridge runs as LocalSystem service and needs to
// read it without depending on which user is logged in.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Dir returns the parent directory of the config file. Created with
// 0o700 if missing so secrets in the JSON don't leak to other users.
func Dir() (string, error) {
	var base string
	switch runtime.GOOS {
	case "windows":
		// LocalSystem service writes to %ProgramData% — survives user
		// logout, no per-profile state.
		base = os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
	default:
		// Dev / macOS — keep state away from $HOME root.
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			xdg = filepath.Join(home, ".config")
		}
		base = xdg
	}
	dir := filepath.Join(base, "esbm-bridge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// Load reads the config file. Returns os.ErrNotExist if the Bridge
// hasn't been paired yet — callers decide whether that's fatal
// (`run`) or expected (`status` from a fresh install).
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if c.BridgeJWT == "" {
		return nil, errors.New("config file present but missing bridge_jwt — re-pair")
	}
	return &c, nil
}

// Save writes the config atomically (temp file + rename) so a crash
// mid-write doesn't leave a corrupt JSON behind.
func (c *Config) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Defaults fills in derived fields the pairing handshake didn't set.
func (c *Config) Defaults() {
	if c.EsbmAppURL == "" {
		c.EsbmAppURL = "https://esbm-app-production.up.railway.app"
	}
	if c.Hostname == "" && c.ShopCode != "" {
		c.Hostname = "bridge-" + sanitize(c.ShopCode)
	}
	if c.TailscaleStateDir == "" {
		dir, _ := Dir()
		c.TailscaleStateDir = filepath.Join(dir, "tailnet")
	}
}

func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+32)
		case r == ' ' || r == '_':
			out = append(out, '-')
		}
	}
	return string(out)
}
