# Windows resources

`../../cmd/bridge/resource_windows_amd64.syso` and
`resource_windows_arm64.syso` embed an application manifest that requests
`requireAdministrator`, so double-clicking `bridge.exe` triggers a UAC
prompt automatically (no terminal). Go links any `*_windows_<arch>.syso` in the
main package automatically, so the cross-compile in the Makefile and
`.github/workflows/release.yml` needs **no change** — the committed `.syso`
files are picked up even when building from Linux/macOS.

Regenerate after editing `versioninfo.json` or `bridge.exe.manifest`:

```sh
go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest
cd cmd/bridge
goversioninfo -64       -manifest ../../packaging/windows/bridge.exe.manifest -o resource_windows_amd64.syso versioninfo.json
goversioninfo -arm -64  -manifest ../../packaging/windows/bridge.exe.manifest -o resource_windows_arm64.syso versioninfo.json
```

Verify a built `.exe` contains the manifest:

```sh
python3 -c "print('requireAdministrator' in open('bridge.exe','rb').read().decode('latin-1'))"
```

Do **not** change the PE subsystem (e.g. `-H windowsgui`): the binary is also a
CLI (`status`, `logs`, the headless `setup` path) and must keep a console for
stdout.
