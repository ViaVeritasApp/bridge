# bridge

A small Go service that runs on an on-prem PC. It watches a folder, syncs every
change to the ViaVeritas CRM over the LAN, serves a small HTTP file API, and
heartbeats so the CRM knows it's online. It installs as a system service and
keeps running across reboots.

## What it does

- **Watches** the folders it syncs and, every scan interval (default 15s), diffs them
  against an on-disk index (`index.txt`) — detecting creates, edits, deletes and
  renames.
- **Syncs** that diff to the ViaVeritas API.
- **Serves** its files to the CRM over the LAN (`GET /files`, `/healthz`), on
  links the API signs.
- **Takes changes** made in the CRM (move, rename, delete) from the API over a
  socket it keeps open, and carries them out on disk.
- **Heartbeats** the API and picks up the TLS certificates it provisions.
- Runs as a **system service** (launchd / systemd / Windows SCM) with
  auto-restart.

## Quick start

Unpack a release anywhere (Downloads is fine), then:

```sh
sudo ./bridge folder add /path/to/files   # as many as you like, none inside another
sudo ./bridge install                # copy into /opt/viaveritas/bridge, register + start the service
sudo ./bridge pair <code>            # --api-url https://api-dev.viaveritas.app outside production
```

On macOS, clear the quarantine flag first: `sudo xattr -dr com.apple.quarantine bridge`.

## Where it lives

`install` copies the program into `/opt/viaveritas/bridge` (`C:\opt\viaveritas\bridge`
on Windows) and registers the service from there, so the folder it was unpacked
in doesn't matter and can be deleted. Its state lives there too: `config.json`
(bridge ID, token, API URL), `index.txt`, `logs/` and `tls/`, **whichever copy
of the program runs**. A copy run again from Downloads finds the same paired
bridge, never a second one:

- `install` from a newer copy updates the installed program (stopping the
  service, replacing it, starting it again) and keeps the pairing and folder;
  from the same version it reinstalls; from an older one it refuses
  (`--force` overrides). `bridge version` prints a program's version.
- The setup wizard says which of those it is and offers Update, and its done
  screen offers it too once the bridge is paired.
- A bridge set up beside its program before installs copied themselves keeps
  working where it is; its next `install` moves its state in.

macOS keeps background services out of Desktop, Documents, Downloads, iCloud
Drive and other disks. A synced folder there needs Full Disk Access for
`/opt/viaveritas/bridge/bridge` (System Settings › Privacy & Security › Full
Disk Access); the wizard says so when such a folder is chosen.

## Folders

A bridge syncs several folders, each a folder of its own inside the bridge's
folder in Files under the name it gives it (its own name, ` 2` when taken):
`/Users/anna/Scans/2026/a.pdf` in the folder called "Scans" is
`/Scans/2026/a.pdf` to the API, in sync, commands and download links alike.

- They never overlap: a folder can't be one synced already, inside one, or
  hold one, nor hold or sit in the bridge's own files (`config.json` would
  sync). Folders are compared with links resolved, ignoring case on macOS and
  Windows. At most 50.
- A file's identity (`inode` to the API) is its folder's id with its inode
  (`<folder id>:<inode>`; the file index on Windows): unique across disks, and
  kept when a drive is mounted again under another device number. The id comes
  from the folder's path. A folder's scan doesn't cross into another disk
  mounted inside it.
- A folder that can't be read (an unplugged drive, one macOS keeps the service
  out of) keeps its files in Files as they were, until it is back.
- Renaming one moves its files in Files (the same files); removing one takes its
  files out of Files and changes nothing on the computer. The portal can't do
  either: a synced folder is the bridge's.
- A file moves only within its folder: commands moving it to another, or
  renaming or deleting a folder itself, are refused (`invalid`).

`bridge folder list|add <path> [--name N]|rename <name|path> <new>|remove <name|path>`
change them; the service reads them at every scan, so no restart is needed.

## Guided setup

`sudo ./bridge setup` (or just `sudo ./bridge`, or a double-click) opens a
local dashboard in the browser, served on `127.0.0.1` with a per-run CSRF
token: a setup guide (folder, install, pairing) until the bridge is set up,
then what it is doing (the service, files in sync, ViaVeritas's last answer,
free space, version, secure address, what needs looking at), its folders (add,
rename and remove them, each with its files and free space) with a preview of
the next sync, its connection and pairing, the live log (searched, filtered
by level and by source, following new lines while scrolled to the end),
and its settings (port, scan interval, hidden files, log folder; Install while
it isn't installed, then Update, Restart and Uninstall). What it runs goes into
the bridge's log, marked `[setup]`, where Activity and `bridge logs` show it.
Headless / SSH installs use the CLI commands.

## Commands

`configure`, `install`, `pair` and `uninstall` require root (Linux/macOS) or
Administrator (Windows); `status`, `preview` and `logs` do not. `--config <path>`
overrides the config location for any command.

| Command | Description |
| --- | --- |
| `folder list\|add\|rename\|remove` | The folders it syncs (see "Folders"). |
| `configure [flags]` | Write `config.json` and restart the service. |
| `install [--force]` | Copy the program into the install folder, register and start the service. |
| `version` | Print the program's version. |
| `pair <code> [--api-url <url>]` | Claim a pairing code and persist the bridge ID/token (API: `https://api.viaveritas.app` unless given). |
| `setup` | Open the guided web wizard. |
| `restart` | Stop the service and start it again. |
| `uninstall` | Stop and remove the service (`config.json`, `index.txt`, `logs/` are kept). |
| `status` | Print pairing, serving URLs, last heartbeat and config paths. |
| `preview` | Show what the next sync would upload, without contacting the API. |
| `resync` | Clear the sync index so every file re-sends on the next scan. |
| `logs [-n N] [--follow]` | Stream the service log (Ctrl+C to stop). |

Common `configure` flags: `--bridge-port <N>` (HTTP API port), `--scan-interval
<S>` (default 15), `--include-hidden`, `--log-dir <DIR>`, `--socket-logs` (stream
the service log over the API socket; the socket itself is always open).

## HTTP API

When `--bridge-port` is set, the service exposes an HTTP API on the LAN. It
changes nothing on disk: it only serves files on links the API signed.

| Endpoint | Description |
| --- | --- |
| `GET /files?path&expires&user_id&sig[&download=1]` | Streams a file. `sig` is the hex HMAC-SHA256 of `path\|expires\|user_id` keyed by the bridge's `signing_key`, valid until `expires` (Unix seconds, an hour after signing). `download=1` sends it as an attachment. |
| `GET /healthz` | `{"ok":true}` while it listens, and nothing else: anyone on the network can ask. |

CORS reflects only the CRM's origins (`allowed_origin`, kept current by the
heartbeat) and answers Chrome's Private Network Access preflight for them. With
a certificate the API serves HTTPS at `https://<lan-ip-dashed>.<subdomain>.<tls_domain>:<port>`,
which public DNS (opendns) resolves to the LAN IP; without one, plain HTTP.

## Changes from the CRM

The bridge keeps a Socket.IO connection to the API's `/bridges` namespace while
it is paired, announcing itself with `health` on connecting and every 30 s. When
a member moves, renames or deletes the bridge's files in the CRM, the API checks
their access and sends a `command`, which the bridge acknowledges with the
outcome of each item:

```json
{"id": "…", "type": "move", "moves": [{"from": "/Scans", "to": "/Old/Scans", "dir": true}]}
{"id": "…", "type": "delete", "paths": ["/Scans/a.pdf"], "prune": "/Scans"}
→ {"items": [{"path": "/Scans", "ok": true}]}
```

Paths are as Files has them, under the synced folder's name. Nothing is overwritten (`exists`), a
missing source is `not_found`, a path outside the synced folders `invalid`, and any
other failure `failed` with the reason. Deleting removes files only; `prune`
then removes the folders left empty, deepest first, so a folder still holding a
hidden or unsynced file stays. Commands wait for a running scan and update
`index.txt` alone: the scan that follows at once finds the change and syncs it,
so Files catches up even when the API never got the answer. One resent with the
same `id` is answered from memory rather than run twice. A move under a name that
starts with a dot is refused while hidden files aren't synced (`--include-hidden`).

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
| `make install [INSTALL_DIR=…]` | Build and copy the binary (default `/opt/viaveritas/bridge`). |
| `make uninstall` / `tidy` / `fmt` / `clean` | As named. |

## Config & files

`config.json` (`0600`), `index.txt` and `logs/` live in the install folder
(see "Where it lives"). Synced folders should be `0755` — don't
`chmod 700` it or the file API can't list it. The service writes uploaded files
`0644` (new dirs `0755`) and never changes the ownership or mode of files already
in the synced folders.

## Releases

`.github/workflows/release.yml` runs on `v*.*.*` tags, and `make package` builds
per-platform archives: macOS/Linux tarballs (the Linux one includes
`bridge.desktop`) and a Windows `.zip` whose `bridge.exe` self-elevates via an
embedded `requireAdministrator` manifest.

The CRM offers the latest release's archives for download through the API
(`GET /bridges/releases/latest`), telling each build by its name,
`bridge_<tag>_<os>_<arch>.tar.gz|zip`: keep the names. A bridge reports its
version (the build's, never the one in `config.json`) and its architecture in
its heartbeat, so the CRM says when it is behind and offers its own build.
