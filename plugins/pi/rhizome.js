// Rhizome relay extension for Pi (>= 1.0). Relay only: it registers one
// `rhizome mcp --agent pi` server per session, proves the session identity over
// a private Unix socket, and executes deliveries. Every decision stays in Go.
//
// Identity: a fresh 128-bit nonce is passed to the MCP child through its
// environment and repeated in the bind frame; the Go side pairs them with the
// Pi process ID in the socket name. The model never supplies an identity.
import crypto from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const MAX_FRAME = 64 * 1024;
const MAX_TEXT = 16 * 1024;
const MAX_BUFFER = 256;
const CONNECT_ATTEMPTS = 50;
const CONNECT_DELAY_MS = 100;
const RECONNECT_LIMIT = 5;

function runtimeDir() {
  return process.env.RHIZOME_RUNTIME_DIR || path.join(os.homedir(), ".rhizome", "run");
}

// `rhizome setup --agents pi` writes rhizome.json next to this file: the absolute
// executable and, only for an explicit configuration file, its path. RHIZOME_BIN
// overrides it; without either, `rhizome` is looked up on PATH.
function launchSettings() {
  try {
    const file = path.join(path.dirname(fileURLToPath(import.meta.url)), "rhizome.json");
    const s = JSON.parse(fs.readFileSync(file, "utf8"));
    return { command: typeof s.command === "string" ? s.command : "", config: typeof s.config === "string" ? s.config : "" };
  } catch {
    return { command: "", config: "" };
  }
}

export function socketPath(runtime, pid, nonce) {
  const hash = crypto.createHash("sha256").update(nonce).digest("hex").slice(0, 12);
  return path.join(runtime, `pi-${pid}-${hash}.sock`);
}

// Text of a Pi content value: a string, or the text blocks of a content array.
function textOf(content) {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content.filter((b) => b && b.type === "text" && typeof b.text === "string").map((b) => b.text).join("\n");
}

function jsonText(v) {
  try { return typeof v === "string" ? v : JSON.stringify(v) ?? ""; } catch { return ""; }
}

// Display only: the model still sees the full peerEnvelope. The renderer shows
// the origin's one-line content the way Claude Code does ("← rhizome: ...").
export function peerView(content) {
  const text = typeof content === "string" ? content : Array.isArray(content)
    ? content.map((c) => (c && c.type === "text" ? c.text : "")).join("") : "";
  const m = /<rhizome-peer-event>\s*([\s\S]*?)\s*<\/rhizome-peer-event>/.exec(text);
  if (m) {
    try {
      const ev = JSON.parse(m[1]);
      if (typeof ev.content === "string") return { body: ev.content, origin: ev.origin ?? {} };
    } catch {}
  }
  return { body: text, origin: {} };
}

// Peer text is untrusted: drop terminal escapes and control characters before
// anything is drawn, so a peer cannot clear the screen, recolor or spoof lines.
export function cleanText(text) {
  return String(text ?? "")
    .replace(/\u001b\][^\u0007\u001b]*(?:\u0007|\u001b\\)?/g, "")
    .replace(/\u001b\[[0-?]*[ -/]*[@-~]/g, "")
    .replace(/\u001b[@-Z\\-_]?/g, "")
    .replace(/\t/g, " ")
    .replace(/\r\n?/g, "\n")
    .replace(/[\u0000-\u0009\u000b-\u001f\u007f-\u009f\u200b-\u200f\u202a-\u202e\u2066-\u2069]/g, "");
}

// Terminal columns of one code point: combining marks 0, wide/emoji 2.
export function cellWidth(ch) {
  if (/^\p{M}$/u.test(ch)) return 0;
  const c = ch.codePointAt(0);
  if (
    (c >= 0x1100 && c <= 0x115f) || (c >= 0x2e80 && c <= 0xa4cf) || (c >= 0xac00 && c <= 0xd7a3) ||
    (c >= 0xf900 && c <= 0xfaff) || (c >= 0xfe30 && c <= 0xfe6f) || (c >= 0xff00 && c <= 0xff60) ||
    (c >= 0xffe0 && c <= 0xffe6) || (c >= 0x1f300 && c <= 0x1faff) || (c >= 0x20000 && c <= 0x3fffd) ||
    /^\p{Extended_Pictographic}$/u.test(ch)
  ) return 2;
  return 1;
}

export function wrap(text, width) {
  const w = Math.max(1, width);
  const out = [];
  for (const line of text.split("\n")) {
    let cur = "";
    let n = 0;
    for (const ch of line) {
      const cw = cellWidth(ch);
      if (n + cw > w && cur) { out.push(cur); cur = ""; n = 0; }
      if (cw > w) continue; // a glyph wider than the terminal cannot be drawn
      cur += ch;
      n += cw;
    }
    out.push(cur);
  }
  return out;
}

function renderPeer(message, options, theme) {
  const view = peerView(message.content);
  const body = cleanText(view.body);
  const origin = view.origin ?? {};
  return {
    invalidate() {},
    render(width) {
      const w = Math.max(1, width - 2);
      // Narrow terminals drop the label so every line still fits.
      const label = w >= 24;
      const pad = label ? 11 : 2;
      const lines = wrap(body, Math.max(1, width - pad));
      const max = options?.expanded ? lines.length : 6;
      const head = label ? theme.fg("accent", "←") + " " + theme.fg("dim", "rhizome:") + " " : theme.fg("accent", "←") + " ";
      const shown = lines.slice(0, max).map((l, i) => (i === 0 ? head : " ".repeat(pad)) + l);
      const extra = [];
      if (lines.length > max) extra.push(`… ${lines.length - max} more lines`);
      if (options?.expanded && origin.session_id) {
        extra.push(`from ${String(origin.session_id).slice(0, 8)} · ${origin.kind ?? "?"} · #${origin.sequence ?? "?"}`);
      }
      for (const e of extra) for (const l of wrap(cleanText(e), w)) shown.push(theme.fg("dim", "  " + l));
      return ["", ...shown];
    },
  };
}

export default function (pi) {
  let current = null; // { sessionId, nonce, cwd, title, socket, closed, queue, dropped }

  function frame(socket, f) {
    const raw = JSON.stringify(f);
    if (raw.length >= MAX_FRAME) return false;
    socket.write(raw + "\n");
    return true;
  }

  // Collected events are relayed raw; every publication decision is made in Go.
  // Until the bind is written they wait in a bounded queue, and what overflows is
  // reported as a gap so subscribers never mistake a hole for silence.
  function emit(s, f) {
    f = { ...f, emitted_at: Date.now() };
    if (!s || s.closed || s !== current) return;
    f = { ...f, sessionID: s.sessionId };
    if (typeof f.text === "string") f.text = f.text.slice(0, MAX_TEXT);
    while (JSON.stringify(f).length >= MAX_FRAME && f.text) f.text = f.text.slice(0, f.text.length >> 1);
    if (s.socket && s.ready) {
      frame(s.socket, f);
      return;
    }
    if (s.queue.length >= MAX_BUFFER) { s.queue.shift(); s.dropped++; }
    s.queue.push(f);
  }

  function flush(s) {
    if (s.dropped > 0) { frame(s.socket, { type: "overflow", sessionID: s.sessionId, dropped: s.dropped }); s.dropped = 0; }
    for (const f of s.queue.splice(0)) frame(s.socket, f);
    s.ready = true;
  }

  function onData(s, buf) {
    let i;
    while ((i = buf.data.indexOf("\n")) >= 0) {
      const line = buf.data.slice(0, i);
      buf.data = buf.data.slice(i + 1);
      if (!line.trim()) continue;
      let f;
      try { f = JSON.parse(line); } catch { continue; }
      if (f.type === "deliver") deliver(s, f);
    }
    if (buf.data.length > MAX_FRAME) s.socket?.destroy();
  }

  function deliver(s, f) {
    const socket = s.socket;
    if (!socket) return;
    if (s !== current || f.sessionID !== s.sessionId || typeof f.text !== "string") {
      frame(socket, { type: "fail", id: f.id, reason: "not the active session" });
      return;
    }
    try {
      // A custom (non-user) message: never a prompt, and idle sessions wake.
      pi.sendMessage(
        { customType: "rhizome", content: f.text.slice(0, MAX_TEXT), display: true },
        { deliverAs: "followUp", triggerTurn: true },
      );
      // The peer-turn marker is emitted when Pi actually feeds the message to the
      // agent (message_end below), not here: a followUp queued behind a running
      // human turn must not suppress that turn's remaining activity.
      frame(socket, { type: "ack", id: f.id });
    } catch (err) {
      frame(socket, { type: "fail", id: f.id, reason: String(err?.message ?? err).slice(0, 200) });
    }
  }

  function connect(s, attempt = 0) {
    if (s.closed || s !== current) return;
    const socket = net.createConnection(socketPath(runtimeDir(), process.pid, s.nonce));
    const buf = { data: "" };
    let up = false;
    socket.setEncoding("utf8");
    socket.on("connect", () => {
      up = true;
      s.socket = socket;
      s.ready = false;
      frame(socket, { type: "hello", version: 1, opencode: "pi" });
      frame(socket, { type: "bind", nonce: s.nonce, sessionID: s.sessionId, directory: s.cwd, title: s.title });
      flush(s);
    });
    socket.on("data", (d) => { buf.data += d; onData(s, buf); });
    socket.on("error", () => {});
    socket.on("close", () => {
      if (s.socket === socket) { s.socket = null; s.ready = false; }
      if (s.closed || s !== current) return;
      const limit = up ? RECONNECT_LIMIT : CONNECT_ATTEMPTS;
      if (attempt + 1 >= limit) return;
      setTimeout(() => connect(s, up ? attempt + 1 : attempt + 1), CONNECT_DELAY_MS);
    });
  }

  function stop(s) {
    if (!s) return;
    s.closed = true;
    s.socket?.destroy();
    s.socket = null;
  }

  pi.registerMessageRenderer?.("rhizome", renderPeer);

  pi.on("session_start", async (_event, ctx) => {
    const sessionId = ctx.sessionManager.getSessionId();
    // Pi can emit session_start twice for one session (RPC mode): same ID is a
    // no-op, a different one replaces the registration with a fresh nonce.
    if (current && !current.closed && current.sessionId === sessionId) return;
    stop(current);
    const s = { sessionId, cwd: ctx.cwd, nonce: crypto.randomBytes(16).toString("hex"), title: "", socket: null, ready: false, queue: [], dropped: 0, closed: false };
    current = s;
    const launch = launchSettings();
    pi.registerMcpServer("rhizome", {
      command: process.env.RHIZOME_BIN || launch.command || "rhizome",
      args: ["mcp", "--agent", "pi", ...(launch.config ? ["--config", launch.config] : [])],
      env: { RHIZOME_PI_NONCE: s.nonce, ...(process.env.RHIZOME_RUNTIME_DIR ? { RHIZOME_RUNTIME_DIR: process.env.RHIZOME_RUNTIME_DIR } : {}) },
      exposure: "direct",
      description: "Rhizome: discover and message peer coding-agent sessions",
    });
    connect(s);
  });

  pi.on("message_end", async (event) => {
    const m = event?.message;
    if (!m) return;
    if (m.role === "custom" && m.customType === "rhizome") {
      // Marks the turn as a reaction to a peer; the next human prompt clears it.
      emit(current, { type: "input", kind: "custom", rhizome: true });
    } else if (m.role === "user") {
      emit(current, { type: "input", kind: "user", text: textOf(m.content) });
    } else if (m.role === "assistant") {
      const text = textOf((m.content ?? []).filter((b) => b?.type === "text"));
      if (text) emit(current, { type: "text", messageID: String(m.responseId ?? m.timestamp ?? ""), text });
    }
  });

  // Nested calls (parentToolCallId) run inside another tool and are not transcript entries.
  pi.on("tool_execution_start", async (event) => {
    if (event?.parentToolCallId) return;
    emit(current, { type: "tool", callID: event.toolCallId, name: event.toolName, phase: "start", text: jsonText(event.args) });
  });

  pi.on("tool_execution_end", async (event) => {
    if (event?.parentToolCallId) return;
    const text = textOf(event.result?.content) || jsonText(event.result);
    emit(current, { type: "tool", callID: event.toolCallId, name: event.toolName, phase: event.isError ? "failed" : "completed", text });
  });

  pi.on("agent_end", async () => {
    emit(current, { type: "turn" });
  });

  pi.on("session_info_changed", async (event) => {
    if (!current || current.closed) return;
    current.title = event.name ?? "";
    if (current.socket) frame(current.socket, { type: "title", sessionID: current.sessionId, title: current.title });
  });

  pi.on("session_shutdown", async () => {
    const s = current;
    current = null;
    stop(s);
    try { pi.unregisterMcpServer("rhizome"); } catch {}
  });
}
