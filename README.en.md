# CodeRelay

A **Windows desktop management tool** for power users: centrally manage CodeBuddy China account pools and run a local **OpenAI-compatible reverse proxy service**, letting clients such as Cursor and CodeBuddy IDE connect to multiple accounts through a single local address with policy-based load balancing, cooldown, and quota scheduling.

![License](https://img.shields.io/badge/license-MIT-blue)
![Version](https://img.shields.io/badge/version-0.1.2-blue)
![Platform](https://img.shields.io/badge/platform-Windows%2010%2F11-lightgrey)

> Visual design follows a "macOS flavor, Windows behavior" philosophy: black/white/gray hierarchy with semantic status colors, sidebar navigation, custom frameless title bar, and a persistent bottom service status bar.

---

## Features

- **Account Pool Management**: Add accounts via OAuth/web login, manual token paste, or config file import. Account health, cooldown, quota, and binding status are visible at a glance.
- **Daily Check-in**: Check in a single account or all accounts with one click.
- **API Key Management**: `sk-*` prefixed keys with binding scope, model restrictions, aliases, and enable/disable.
- **Model Management**: Sync model lists from the online API with local caching; supports aliases and disabling.
- **Local Reverse Proxy Service**: `127.0.0.1:11435`, manual start/stop, multi-account scheduling strategies with session affinity.
- **Request Statistics & Logs**: Total requests, tokens, cache hit rate, credit consumption, hourly bar chart; logs retained for 7 days with filtering and JSON export.
- **System Tray & Notifications**: Minimize to tray, start/stop proxy, quit; Windows system notifications on startup failure or service errors.

---

## Quick Start

### Prerequisites

- Node.js >= 20 (20+ recommended)
- Rust / cargo >= 1.8x
- Go >= 1.22
- Tauri CLI (provided as npm devDependency, no global install needed)

### Development

```powershell
npm install
npm run tauri:dev
```

### Production Build

```powershell
npm run tauri:build
```

Build artifacts are output to `F:\target\release\bundle\` (NSIS installer and MSI).

> Note: This project's Cargo target directory is configured as `F:\target`, not the default `src-tauri/target`.

---

## Usage

1. **Add Accounts**: Go to "Account Pool" and add CodeBuddy China accounts via browser authentication, manual token, or config file import.
2. **Create an API Key**: On the "API Key" page, create a `sk-` prefixed key and bind it to the available account scope.
3. **Start the Service**: Manually start the reverse proxy from "Service Config" or the bottom status bar (the service does **not** auto-start with the app).
4. **Connect Your Client**: Point your client to `http://127.0.0.1:11435` and authenticate with the API key you created.

### Connection Examples

**OpenAI-compatible base_url**:

```
http://127.0.0.1:11435/v1
```

**curl**:

```bash
curl http://127.0.0.1:11435/v1/chat/completions \
  -H "Authorization: Bearer sk-xxxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"Hello"}]}'
```

---

## Architecture

```
+----------------------------------------------+
|  Frontend   React 19 + TypeScript + Vite     |
|        (zustand state / lucide-react icons)   |
+----------------------+-----------------------+
                       | Tauri invoke / events
+----------------------+-----------------------+
|  Desktop Shell   Tauri 2 + Rust             |
|        (gateway.rs: sidecar process mgmt /    |
|         state machine)                        |
+----------------------+-----------------------+
                       | Launch sidecar + stdout events
+----------------------+-----------------------+
|  Reverse Proxy Sidecar   Go                  |
|        (embedded CLIProxyAPI v7.2.140 +       |
|         CodeBuddy CN patch,                  |
|         listens on 127.0.0.1:11435)           |
+----------------------------------------------+
```

**Data Flow**: Rust writes account credentials to a temporary `auths/` directory and generates `config.json`/`manifest.json` -> launches the sidecar -> the sidecar sends structured lifecycle and request events via **stdout (JSON lines)** -> Rust parses and updates state -> notifies the frontend to refresh via events.

---

## Developer Guide

### Directory Structure

| Path | Responsibility |
|---|---|
| `src/` | React frontend; `App.tsx` hosts main pages; `services.ts` wraps invoke/HTTP; `types.ts` types |
| `src-tauri/src/lib.rs` | Tauri entry point (plugins, single instance, tray, window) |
| `src-tauri/src/gateway.rs` | Core: sidecar process management, state machine, event parsing, credential hot-reload |
| `src-tauri/src/codebuddy_oauth.rs` | Account OAuth, token/quota refresh, check-in |
| `src-tauri/src/models.rs` | Request log / statistics structures |
| `sidecars/coderelay-proxy/` | Go sidecar main program (relay server, model sync, account pool scheduling) |
| `scripts/` | `build-sidecar.ps1`, `sync-version.mjs` |

### Common Commands

```powershell
npm run typecheck          # Frontend TS type check
npm run build              # Frontend production build
npm run build:sidecar      # Build Go sidecar for Rust target triple
npm run sync-version       # Sync package.json version to tauri.conf.json/Cargo.toml
cargo check --manifest-path src-tauri/Cargo.toml
go build ./...             # under sidecars/coderelay-proxy
go test ./...
```

### Version Convention

**Single version source = `package.json` `version`**. To bump the version, modify only this file, then run `npm run sync-version` (or just `tauri:build`, which includes it in beforeBuild). The frontend displays the version via `APP_VERSION` (injected by Vite) — do not hardcode it.

---

## Third-Party Components & Licenses

The reverse proxy sidecar is built on [CLIProxyAPI](https://github.com/) (MIT License). For third-party license and attribution information, see:

- `NOTICE.md`
- `sidecars/coderelay-proxy/third_party/CLIProxyAPI/LICENSE`

---

## License

This project is open-sourced under the [MIT License](./LICENSE).

---

## Disclaimer

CodeRelay is an independent third-party tool and is not affiliated with Tencent or CodeBuddy. Please comply with the terms of service of CodeBuddy and related upstream services, and use it only for lawful, compliant learning and personal purposes. Account credentials, tokens, and API keys are stored locally only — this project does not collect or upload any credentials.
