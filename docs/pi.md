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
| `rhizome setup` / `uninstall`, skill install | Not implemented |
| Paired-host gateway (`pi` sessions are not exported) | Not implemented |

## Manual install (until `setup` supports Pi)

1. `task build`.
2. Copy `plugins/pi/rhizome.js` to `~/.pi/agent/extensions/rhizome/index.js`
   (or under `$PI_CODING_AGENT_DIR/extensions/`).
3. Start Pi with `RHIZOME_BIN=/absolute/path/to/rhizome` in its environment; the
   extension registers `rhizome mcp --agent pi` itself, so no `mcp.json` entry is
   needed. If `runtime_dir` is set in `~/.rhizome/config.toml`, also set the same
   `RHIZOME_RUNTIME_DIR` for Pi, because the extension cannot read the file.

Without the extension nothing binds: `rhizome mcp --agent pi` exits because
`RHIZOME_PI_NONCE` is missing.

## Verify

`go test -race ./internal/channel -run TestPi` and `python3 scripts/smoke_pi.py`
(real `pi --mode rpc`, no model turn, no credentials).
