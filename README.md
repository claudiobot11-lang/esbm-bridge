# esbm-bridge

Windows-first agent that bridges an in-store **eRetail BLE gateway**
(Zhsunyco S-ETAP05) to the **ESBM cloud eRetail** running on Hetzner.

```
┌─ STORE ─────────────────────────────┐         ┌─ CLOUD ──────────┐
│                                     │         │                  │
│  ESL tags ──BLE──> S-ETAP05 ────┐   │  VPN    │  eRetail Docker  │
│                                 ▼   ├─────────┤  (Hetzner VPS)   │
│                          esbm-bridge│ (Tailscale)│ port 9071    │
│                          (this app) │         │  on tailscale0   │
│                                     │         │                  │
└─────────────────────────────────────┘         └─────────▲────────┘
                                                          │
                                                  esbm-app (Railway)
                                                  /esl/stores panel
```

## Why it exists

The S-ETAP05 gateway is LAN-only — it can only talk to an MQTT broker
on its local network. To run eRetail in the cloud and operate ESLs from
multiple physical stores, every store needs a small device that:

- joins the ESBM private Tailscale network
- listens locally on port 9071 for the gateway
- forwards traffic to the cloud eRetail through the VPN
- reports its health to the esbm-app dashboard

`esbm-bridge` is that device, packaged as a single binary that runs as
a Windows service on the store's existing PC (the one that already
runs Vori POS). No new hardware purchase per store.

## Status

Pre-MVP skeleton. Pairing flow + MQTT proxy in `cmd/bridge/`. Tray UI,
MSI installer, auto-update planned — see roadmap below.

## Build

```bash
go build -o bin/esbm-bridge ./cmd/bridge
```

Cross-compile for Windows from macOS:
```bash
GOOS=windows GOARCH=amd64 go build -o bin/esbm-bridge.exe ./cmd/bridge
```

## CLI usage

```bash
# Pair with esbm-app using the 6-digit code shown in /esl/stores
esbm-bridge pair --code 482739 --server https://esbm-app-production.up.railway.app

# Start the bridge service (foreground for now; Windows service wrapper later)
esbm-bridge run

# Print current state
esbm-bridge status
```

## Configuration

Persisted to:
- Windows: `%ProgramData%\esbm-bridge\config.json`
- macOS/Linux (dev): `~/.config/esbm-bridge/config.json`

Fields written by `pair`:
- `bridge_jwt` — long-lived token used to authenticate to esbm-app
- `ts_auth_key` — Tailscale ephemeral key for this device
- `shop_code` — the eRetail shopCode assigned to this store
- `server_addr` — eRetail Tailscale IP:port (e.g. 100.108.175.3:9071)
- `esbm_app_url` — esbm-app base URL for heartbeats

## Roadmap

- [ ] **MVP**: pair + run subcommands, TCP proxy on 9071, Tailscale via `tsnet`
- [ ] Heartbeat loop with backoff, reports gateway counts + MQTT rate
- [ ] Windows service wrapper (`golang.org/x/sys/windows/svc`)
- [ ] Systray icon (`getlantern/systray`) with status + manual restart
- [ ] MSI installer (WiX or go-msi), code-signed
- [ ] Auto-update via signed manifest from esbm-app
- [ ] Diagnostic command bundles logs + tailscale netcheck into a zip
- [ ] Multi-gateway: one Bridge handles N gateways per LAN

See `../Claudiobot/ESBM/ESL/09 - ESBM Bridge (multi-loja).md` for the full
product plan and integration design.
