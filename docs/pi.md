# Pi integration (in progress)

Targets Pi **1.4.0** (`@earendil-works/pi-coding-agent`); earlier versions without
built-in MCP (before 0.99.0) are unsupported. Design, research and spike results:
[docs/superpowers/specs/2026-10-28-pi-support-design.md](superpowers/specs/2026-10-28-pi-support-design.md).

## Status

| Capability | State |
| --- | --- |
| Verified binding (nonce + parent PID over a private socket) | Implemented |
| `list_sessions` shows the live Pi session; Pi native title (`session_info`) | Implemented (title unverified live) |
| Peer delivery as `pi.sendMessage` custom messages, with acknowledgement | Implemented in Go and the extension; live delivery into a real session is only covered by the design spike |
| Activity collection, history source, `recent` rows | Not implemented |
| `rhizome setup` / `uninstall --agents pi`, skill install | Implemented |
| Paired-host gateway (`pi` sessions are not exported) | Not implemented |

## Install

```sh
rhizome setup --agents pi      # skill + extension; add --list to preview
rhizome uninstall --agents pi  # removes only those Rhizome-owned files
```

Setup writes, under `$PI_CODING_AGENT_DIR` (default `~/.pi/agent`):

- `skills/rhizome-collab/`, the collaboration skill;
- `extensions/rhizome/index.js`, the embedded extension (reinstalled on every
  upgrade so it matches the binary's bridge protocol);
- `extensions/rhizome/rhizome.json`, generated: the absolute executable and,
  only when you selected a configuration with `--config`/`RHIZOME_CONFIG`, its
  path. It never holds a token.

Pi's own `settings.json` and `mcp.json` are never edited: the extension registers
`rhizome mcp --agent pi` for each session itself. Restart Pi (or `/reload`)
afterwards. A symlinked extension or skill is left alone, so a checkout you manage
keeps working; `uninstall` skips it as well.

`RHIZOME_BIN` in Pi's environment overrides the pinned executable. If `runtime_dir`
is set in `~/.rhizome/config.toml`, set the same `RHIZOME_RUNTIME_DIR` for Pi,
because the extension cannot read that file.

Without the extension nothing binds: `rhizome mcp --agent pi` exits because
`RHIZOME_PI_NONCE` is missing.

## Verify

`go test -race ./internal/channel -run TestPi` and `python3 scripts/smoke_pi.py`
(real `pi --mode rpc`, no model turn, no credentials).
