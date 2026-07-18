# bridge

A small Go service that runs on an on-prem PC. It watches a folder, syncs every
change to the ViaVeritas CRM over the LAN, serves a small HTTP file API, and
heartbeats so the CRM knows it's online. It installs as a system service and
keeps running across reboots.

## What it does

- **Watches** a data folder and, every scan interval (default 15s), diffs it
  against an on-disk index (`index.txt`) — detecting creates, edits, deletes and
  renames.
- **Syncs** that diff to the ViaVeritas API.
- **Serves** an HTTP API (`/files`, `/healthz`) for upload, download, delete and
  rename.
- **Heartbeats** the API and picks up the TLS certificates it provisions.
- Runs as a **system service** (launchd / systemd / Windows SCM) with
  auto-restart.

## Quick start

```sh
sudo make install                    # build → /opt/bridge
cd /opt/bridge
sudo ./bridge configure --data-root /path/to/files
sudo ./bridge install                # register + start the service
sudo ./bridge pair <code> --api-url https://api.viaveritas.app/
```

`pair` writes `config.json` (bridge ID, token, API URL) next to the binary; the
service auto-starts on boot from then on.

On macOS, clear the quarantine flag first:
`sudo xattr -dr com.apple.quarantine /opt/bridge/bridge`.

## Guided setup

Non-technical operators can run `sudo ./bridge setup` (or just `sudo ./bridge`)
to open a local browser wizard — served on `127.0.0.1` with a per-run CSRF token
— that walks through folder selection, install and pairing. Release archives
also ship a double-click launcher that shows the OS's native admin prompt and
then opens the wizard. Headless / SSH installs fall back to the CLI commands.

## Commands

`configure`, `install`, `pair` and `uninstall` require root (Linux/macOS) or
Administrator (Windows); `status`, `preview` and `logs` do not. `--config <path>`
overrides the config location for any command.

| Command | Description |
| --- | --- |
| `configure --data-root <path> [flags]` | Write `config.json` and restart the service. |
| `install` | Register and start the system service. |
| `pair <code> --api-url <url>` | Claim a pairing code and persist the bridge ID/token. |
| `setup` | Open the guided web wizard. |
| `uninstall` | Stop and remove the service (`config.json`, `index.txt`, `logs/` are kept). |
| `status` | Print pairing, serving URLs, last heartbeat and config paths. |
| `preview` | Show what the next sync would upload, without contacting the API. |
| `resync` | Clear the sync index so every file re-sends on the next scan. |
| `logs [-n N] [--follow]` | Stream the service log (Ctrl+C to stop). |

Common `configure` flags: `--bridge-port <N>` (HTTP API port), `--scan-interval
<S>` (default 15), `--include-hidden`, `--log-dir <DIR>`, `--socket-logs`.

## HTTP API

When `--bridge-port` is set, the service exposes an HTTP API. File endpoints
authorise against the upstream API (the bridge doesn't authenticate at this layer
— the API is the source of truth); the bridge identifies itself with
`X-Client-Id` / `X-Client-Secret` from `config.json`, and forwards a caller's
`Authorization` header when present.

| Endpoint | Description |
| --- | --- |
| `POST /files` | Multipart upload (`metadata` + `file` parts), synced to the API. |
| `GET /files?…` | Streamed download from an HMAC-signed URL the API issued. |
| `DELETE /files?path` | Remove from disk and notify the API. |
| `PATCH /files?path` | Rename on disk and notify the API. |
| `GET /healthz` | Readiness probe. |

## Troubleshooting: files not appearing in the CRM

The bridge tracks sent files in `index.txt` and uploads only the diff. If that
index is stale (it lists files the server never stored), the bridge thinks it's
caught up and stays silent.

- `bridge logs` prints a `[watch]` line per scan; `bridge preview` shows the
  planned sync without calling the API.
- `sudo bridge resync` (or the wizard's **Force full re-sync**) clears the index
  so the full inventory re-sends.
- `BRIDGE_DEBUG=1` enables verbose logging.

## Build

Requires **Go 1.23+** (pure Go, `CGO_ENABLED=0`).

| Target | Description |
| --- | --- |
| `make build` | Build a single binary for the current host. |
| `make build-all` | Cross-compile all platforms into `dist/`. |
| `make package` | `build-all` plus the shippable archives. |
| `make install [INSTALL_DIR=…]` | Build and copy the binary (default `/opt/bridge`). |
| `make uninstall` / `tidy` / `fmt` / `clean` | As named. |

## Config & files

`config.json` (`0600`), `index.txt` and `logs/` all live next to the binary, so a
stable install directory matters. The data root should be `0755` — don't
`chmod 700` it or the file API can't list it. The service writes uploaded files
`0644` (new dirs `0755`) and never changes the ownership or mode of files already
in the data root.

## Releases

`.github/workflows/release.yml` runs on `v*.*.*` tags, and `make package` builds
per-platform archives: macOS/Linux tarballs (the Linux one includes
`bridge.desktop`) and a Windows `.zip` whose `bridge.exe` self-elevates via an
embedded `requireAdministrator` manifest.
