// Rhizome relay extension for Pi (>= 1.0). Relay only: it registers one
// `rhizome mcp --agent pi` server per session, proves the session identity over
// a private Unix socket, and executes deliveries. Every decision stays in Go.
//
// Identity: a fresh 128-bit nonce is passed to the MCP child through its
// environment and repeated in the bind frame; the Go side pairs them with the
// Pi process ID in the socket name. The model never supplies an identity.
import crypto from "node:crypto";
import net from "node:net";
import os from "node:os";
import path from "node:path";

const MAX_FRAME = 64 * 1024;
const MAX_TEXT = 16 * 1024;
const CONNECT_ATTEMPTS = 50;
const CONNECT_DELAY_MS = 100;
const RECONNECT_LIMIT = 5;

function runtimeDir() {
  return process.env.RHIZOME_RUNTIME_DIR || path.join(os.homedir(), ".rhizome", "run");
}

export function socketPath(runtime, pid, nonce) {
  const hash = crypto.createHash("sha256").update(nonce).digest("hex").slice(0, 12);
  return path.join(runtime, `pi-${pid}-${hash}.sock`);
}

export default function (pi) {
  let current = null; // { sessionId, nonce, cwd, title, socket, closed }

  function frame(socket, f) {
    const raw = JSON.stringify(f);
    if (raw.length >= MAX_FRAME) return false;
    socket.write(raw + "\n");
    return true;
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
      frame(socket, { type: "hello", version: 1, opencode: "pi" });
      frame(socket, { type: "bind", nonce: s.nonce, sessionID: s.sessionId, directory: s.cwd, title: s.title });
    });
    socket.on("data", (d) => { buf.data += d; onData(s, buf); });
    socket.on("error", () => {});
    socket.on("close", () => {
      if (s.socket === socket) s.socket = null;
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

  pi.on("session_start", async (_event, ctx) => {
    const sessionId = ctx.sessionManager.getSessionId();
    // Pi can emit session_start twice for one session (RPC mode): same ID is a
    // no-op, a different one replaces the registration with a fresh nonce.
    if (current && !current.closed && current.sessionId === sessionId) return;
    stop(current);
    const s = { sessionId, cwd: ctx.cwd, nonce: crypto.randomBytes(16).toString("hex"), title: "", socket: null, closed: false };
    current = s;
    pi.registerMcpServer("rhizome", {
      command: process.env.RHIZOME_BIN || "rhizome",
      args: ["mcp", "--agent", "pi"],
      env: { RHIZOME_PI_NONCE: s.nonce, ...(process.env.RHIZOME_RUNTIME_DIR ? { RHIZOME_RUNTIME_DIR: process.env.RHIZOME_RUNTIME_DIR } : {}) },
      exposure: "direct",
      description: "Rhizome: discover and message peer coding-agent sessions",
    });
    connect(s);
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
