# bridge

A small Go service that connects a computer or a server to ViaVeritas. It does
what its features allow there: today, syncing the folders chosen on it to Files,
serving their files on the LAN, saying what the computer is and how it is doing,
measuring its way to ViaVeritas, telling what happens to it, updating itself to
signed releases, and running terminals and commands from ViaVeritas when the
computer allows them. It heartbeats so ViaVeritas knows it's
online, installs as a system service and keeps running across reboots. Syncing
a folder is optional.

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
- **Reports** what the computer is (inventory) and how it is doing (metrics), and
  runs speedtests to ViaVeritas when asked (see "Monitoring").
- **Runs terminals and commands** opened from ViaVeritas, as who the computer
  chose, while it allows them (see "Terminals and commands").
- **Updates itself** to releases ViaVeritas signed when someone asks, and goes
  back to the version before when a new one doesn't reach ViaVeritas (see
  "Updates"); always says when it starts, stops or crashes (see "Telemetry").
- Runs as a **system service** (launchd / systemd / Windows SCM) with
  auto-restart.
- Each of its **features** is switched on this computer and in ViaVeritas, and
  runs only while both allow it (see "Features").

## Quick start

Unpack a release anywhere (Downloads is fine), then:

```sh
sudo ./bridge install                # copy into /opt/viaveritas/bridge, register + start the service
sudo ./bridge pair <code>            # --api-url https://api-dev.viaveritas.app outside production
sudo ./bridge folder add /path/to/files   # optional: as many as you like, none inside another
```

A service installed before pairing waits for it, and starts working the moment
the bridge is paired.

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
change them; the service notices within seconds and scans at once, so no
restart is needed. A bridge without folders syncs nothing, and Files shows no
folder for it.

## Features

What the bridge does is a list of features (`internal/config/features.go`; the
API's `bridge-features.ts` and the portal's `BridgeFeatures` keep the same):
`files`, `inventory`, `inventory_personal`, `metrics`, `speedtest`, `logs`,
`terminal` and `proxy`. A version lists only those it has (`Built`), and
reports them with every heartbeat among its capabilities, with `updates` when it
updates itself (no switch: see "Updates"). Telemetry is no switch either: it is
always on.

- **Two switches.** Each is switched on this computer (`features` in
  `config.json`; the dashboard's Features view or `bridge features
  enable|disable <feature>`) and in ViaVeritas (`portal_features`, as the
  heartbeat's answer or a `features` socket event last said). It runs only while
  both allow it: switched off here, ViaVeritas can't turn it on.
- **Starting off here**: `inventory_personal` (who is signed in) and the remote
  ones; the rest start on.
- **The remote ones** (logs, terminal, proxy) have someone act on the
  computer from ViaVeritas: they start off here, and `bridge pause` stops them
  all at once, whatever the switches say, until `bridge resume`.
- **Files off** stops syncing the folders, refuses commands and downloads; the
  folders and their index stay as they were.
- The service reads its settings again within two seconds of a change (no
  restart), and closes whatever a feature switched off had open.

## Guided setup

`sudo ./bridge setup` (or just `sudo ./bridge`, or a double-click) opens a
local dashboard in the browser, served on `127.0.0.1` with a per-run CSRF
token: a setup guide (install, pairing, and folders if wanted) until the bridge is set up,
then what it is doing (the service, files in sync, ViaVeritas's last answer,
free space, version, secure address, what needs looking at), its folders (add,
rename and remove them, each with its files and free space) with a preview of
the next sync, its connection and pairing (with its socket as the running
service says), its features (both switches, pausing remote access, who terminals run as, what
the logs ViaVeritas may read, and what ViaVeritas has open on the computer,
each with End), the live log (searched, filtered
by level and by source, following new lines while scrolled to the end),
and its settings (port, scan interval, hidden files, log folder; Install while
it isn't installed, then Update, Restart and Uninstall). What it runs goes into
the bridge's log, marked `[setup]`, where Activity and `bridge logs` show it.
Headless / SSH installs use the CLI commands.

## Commands

`configure`, `install`, `pair`, `uninstall`, `folder add|rename|remove`, `features
enable|disable`, `pause`, `resume`, `sessions`, `updates` and `log-sources` require root
(Linux/macOS) or Administrator (Windows); `status`, `preview`, `features list`
and `logs` do not. `--config <path>`
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
| `features list\|enable\|disable <feature>` | Its features: this computer's switch, ViaVeritas's, and whether each runs. |
| `pause` / `resume` | Pause or resume all remote access. |
| `sessions list\|end <id>` | What ViaVeritas has open on this computer now, and ending it. |
| `sessions run-as user\|system` | Who terminals and commands run as (see "Terminals and commands"). |
| `sessions notices on\|off` | Whether this computer's screen says when one starts. |
| `updates [now]` | Whether a newer version is out, and update to it now (see "Updates"). |
| `log-sources list\|add <kind> [target] [--name]\|remove <id>` | The logs ViaVeritas may read live (see "Logs"). |

Common `configure` flags: `--bridge-port <N>` (HTTP API port), `--scan-interval
<S>` (default 15), `--include-hidden`, `--log-dir <DIR>`.

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

The socket is checked once, as it connects: a refusal (`unauthorized`, a
deleted bridge) waits five minutes before trying again, anything else 5 to 30 s.

Paths are as Files has them, under the synced folder's name. Nothing is overwritten (`exists`), a
missing source is `not_found`, a path outside the synced folders `invalid`, and any
other failure `failed` with the reason. Deleting removes files only; `prune`
then removes the folders left empty, deepest first, so a folder still holding a
hidden or unsynced file stays. Commands wait for a running scan and update
`index.txt` alone: the scan that follows at once finds the change and syncs it,
so Files catches up even when the API never got the answer. One resent with the
same `id` is answered from memory rather than run twice. A move under a name that
starts with a dot is refused while hidden files aren't synced (`--include-hidden`).

## Monitoring

Everything here travels over the socket to ViaVeritas, never the LAN, and only
while its feature runs.

- **Inventory** (`internal/inventory`, gopsutil): the computer (name, model,
  serial, system and build, kernel, virtualization, time zone, boot time), its
  processor, memory, disks (on macOS only `/` and what is under `/Volumes`) and
  network (interfaces with an address that isn't link-local, MACs, gateway,
  DNS), plus who is signed in with `inventory_personal`. Sent as `inventory
  {hash, inventory}` when the socket connects and whenever the hash changes; it
  looks every hour and whenever a switch changes.
- **Metrics** (`internal/metrics`): a sample every 30 seconds (processor, load,
  memory, swap, each disk's use and free space, disk and network rates per
  device, the warmest sensor), each sent at once as `metrics {samples}` (240 at
  most a message). While the socket is down they wait in memory, a day's worth
  at most, and go once it is back. `live {seconds}` from the API asks for a
  sample every two seconds for that long, sent the same way: ViaVeritas keeps
  every sample with its time and shows the newest to whoever watches; the
  portal asks again while someone does.
- **Speedtest** (`internal/speedtest`): `speedtest {token, seconds, max_bytes,
  streams}` from the API runs one (one at a time): ten round trips to
  `GET /bridges/speedtest/ping`, then four parallel transfers down
  (`GET /bridges/speedtest/download?bytes=`) and up
  (`POST /bridges/speedtest/upload`), each for a few seconds and at most 25 MB,
  answered with latency, jitter and both rates. The token is good for that one
  test.

## Updates

`internal/selfupdate`. The bridge installs only a release whose `SHA256SUMS.txt`
is signed (ed25519, `SHA256SUMS.txt.sig`) by a key compiled into it
(`internal/selfupdate/keys.go`); a build without a key never updates itself,
and neither does a developer's build (a version that isn't `vX.Y.Z`) or a copy
that isn't the installed one.

1. ViaVeritas offers the latest release that suits this computer: in the
   heartbeat's answer (`update`, kept and shown, never installed by itself), or
   in an `update` socket event when someone asks for it now (answered at once:
   `{ok}` or `{ok: false, code, message}`).
2. **When**: only when someone asks, in ViaVeritas or here (the dashboard,
   `bridge updates now`). Nothing updates by itself.
3. It downloads the checksums, the signature and the archive into
   `updates/<version>/`, checks the signature and the archive's checksum, makes
   sure the archive is the build of that version for this system, takes the
   program out and runs `version` on it.
4. It renames the running program to `bridge.previous` (`bridge.previous.exe`),
   puts the new one in its place, and ends with code 3 so the service manager
   starts it again (Windows, launchd and systemd all do).
5. The new version has two minutes to reach ViaVeritas (a heartbeat or the
   socket), and two starts: past either, `bridge.previous` goes back and starts.

`update-state.json` keeps how it stands across the restart; every heartbeat
says it (`update`), and a change of phase sends one at once. `bridge updates`
and the dashboard's Remote access view show it, with the newer version out.

## Terminals and commands

`internal/terminal`, stream kinds `terminal` and `exec`, behind the remote
feature `terminal` (off here until switched on, and held by `bridge pause`): a
shell and a command both run anything on the computer as the same account, so
one switch covers both.

- **Who they run as** is this computer's choice (`run_as`; `bridge sessions
  run-as user|system`, or the dashboard): the person signed in at it (`user`,
  the default: on macOS the owner of the console, on Linux logind's active local
  session, on Windows the console session's user, whose identity the service
  takes from Windows with `WTSQueryUserToken`), or its administrator (`system`:
  root, or SYSTEM). The bridge never keeps a password; with `user` and nobody
  signed in, a session is refused before it opens (`unavailable`). A bridge not
  running as root (a developer's run) runs them as itself.
- **A terminal** is the account's login shell (PowerShell on Windows) in a
  pseudo-terminal (`go-pty`: PTY, ConPTY on Windows), sized as ViaVeritas says
  and resized with it. It ends when the shell exits (with its exit code), when
  ViaVeritas closes it, after 15 minutes without input or output (`idle`), or
  after four hours (`max_time`). Three at most at once.
- **A command** (`exec`, `{command, timeout_seconds}`) runs in the account's
  login shell (`sh -lc`, `powershell -Command`), its output and errors streamed
  back as they come, then its exit code; past its time (five minutes unless
  asked, an hour at most) it is stopped (`timeout`). Four at most at once.
- When one opens, the bridge tells ViaVeritas who it runs as (`stream_info`),
  logs it (`[remote]`), and says it on this computer's screen unless switched
  off (`bridge sessions notices off`): a notification on macOS and Linux, a
  message box on Windows. Ending one ends what it started (its process group).
- What a session shows is kept by ViaVeritas for its replay; what is typed
  never is.

## Logs

`internal/logs`, stream kind `logs`, behind the remote feature `logs` (off here
until switched on, and held by `bridge pause`). ViaVeritas reads, live, only the
logs this computer lists (`log_sources` in `config.json`; `bridge log-sources
add|remove`, or the dashboard's Features view), and names one by its id, never
by a path. Until someone edits the list here, it is the system's usual logs that
exist on this computer (`DefaultLogSources`, each under an id that stays the
same): the system journal, `/var/log/syslog` or `/var/log/messages`, and Nginx's
and Apache's logs on Linux; the unified log and `/var/log/install.log` on macOS;
the System and Application event logs, and IIS's, on Windows. Once edited, the
list is kept as edited, so one taken off stays off.

- `bridge`: the bridge's own log, always listed.
- `file <path or pattern>`: a file, or the newest file a pattern matches
  (`/var/log/nginx/*.log`), followed as it is written: cut back, it is read from
  its start; a newer file matching the pattern is taken up in its place.
- `journal [unit]` (Linux): systemd's journal, every unit's or one's.
- `macos [predicate]` (macOS): the unified log, everything or what a
  `log stream --predicate` keeps.
- `eventlog <name>` (Windows): an event log (System, Application…), read through
  PowerShell every two seconds.

A view (`{source, lines, follow, level, query}`) sends the newest `lines` (200
unless asked, 2,000 at most), then what is written, one JSON line an entry
(`{ts, level, text, origin}`, `note` for the bridge's own lines about the view),
kept to `level` and above and holding `query` (any case), filtered here. A new
filter from ViaVeritas (the same JSON, without `source`) reads it again after a
`{"reset":true}` line. A level is `debug`, `info`, `warn` or `error`: the log's own
where it says, guessed from the words of a plain file's line. A view ends with its
stream, when its log is taken off the list (`removed`), or after four hours; one it
can't read now closes as `source_unavailable`, and an id the list doesn't hold is
refused (`unknown_source`). Four at once. Nothing is kept: ViaVeritas keeps who
read which log and when, not what it showed.

## Connections into the network

`internal/proxy`, stream kinds `http` and `tcp`, behind the remote feature
`proxy` (off here until switched on, and held by `bridge pause`). While it runs,
ViaVeritas reaches anything this computer can, on its network or beyond: no list
is kept, and switching it off (here or in ViaVeritas) is what stops it, closing
what is open. What is dialled is the address the name resolved to here.

- **`http`** (`{method, url, headers, body_bytes, timeout_seconds, tls_pin}`): one
  request, its body the first `body_bytes` the API sends (10 MB at most), its
  answer streamed back as a JSON line (`{status, headers, address, tls}`) and
  then the body. Redirects come back as they are, never followed; hop-by-hop
  headers stay out. HTTPS is checked against the system's authorities, or, when
  the request gives a pin (`tls_pin`, the certificate's SHA-256), only against it.
  Thirty seconds unless asked, two minutes at most. Four at once.
- **`tcp`** (`{host, port}`): bytes both ways until either side ends; the
  address reached goes back as `stream_info`. Four at once.
- Failures close with `unreachable` (no answer, or a name that doesn't resolve
  here), `tls` (an untrusted certificate) or `timeout`. The log keeps who asked
  and where, without a request's query.

## Telemetry

`internal/telemetry`. What happens to the bridge itself is kept as events in
`events.jsonl` (200 at most) until ViaVeritas acknowledges them: `events
{items}` on the socket, answered `{stored}`. Kinds: `started` (with how the run
before ended: `clean`, `crash`, or `unclean` when it stopped without saying so),
`stopped`, `crashed` (the Go runtime's trace, from `crash.log` through
`debug.SetCrashOutput`, with this computer's paths taken out), and
`update_started`, `update_installed`, `update_failed`, `update_rolled_back`.
Always on: no switch here or in ViaVeritas holds them.

## Streams

What goes on for a while (a connection test, a terminal, a command's output, a
connection into the network, a log read live) is a stream on the same socket
(`internal/streams`). The API opens one with `stream_open {id, kind, user,
params, window}`, acked `{ok, window}` or `{ok: false, code, message}`; both
sides then send `stream_data {id, seq, d}` (base64, at most 32 KiB, numbered in
order: the bridge reorders what comes early), grant room with `stream_ack {id,
bytes}` (256 KiB at most unanswered; `reset` frees it all), and end it with
`stream_close {id, code, message}`. A kind whose feature doesn't run is refused
(`feature_off`, `paused`), at most 8 are open at once, and they all end with the
socket. Control messages (pongs, answers, acks) always go ahead of stream data.
The running service lists them for the dashboard and the CLI through
`control.json` in its folder: a loopback port and a token only administrators
can read.

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

`config.json` (`0600`), `index.txt`, `control.json` (`0600`, while the service
runs), `update-state.json`, `events.jsonl`, `crash.log`, `updates/`,
`bridge.previous` and `logs/` live in the install folder (see "Where it lives"). On Windows
the install folder is SYSTEM and Administrators only, set at install and at
every start of the service. Synced folders should be `0755` — don't
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

**Signing.** The release job signs `SHA256SUMS.txt` with the
`BRIDGE_UPDATE_SIGNING_KEY` secret (`go run ./tools/sign sign`) and publishes
`SHA256SUMS.txt.sig`; without the secret it warns and publishes the release
unsigned, and no bridge updates to it. `go run ./tools/sign keygen`
makes a key pair: the private half goes into the secret, the public half into
`internal/selfupdate/keys.go` (two keys there while one replaces the other).
`go run ./tools/sign verify` checks a release by hand.
