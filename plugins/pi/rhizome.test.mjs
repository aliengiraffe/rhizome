import { test } from "node:test"
import assert from "node:assert/strict"
import { createServer } from "node:net"
import { mkdtempSync, mkdirSync } from "node:fs"
import { join } from "node:path"

const root = mkdtempSync("/tmp/rzpi-")
process.env.RHIZOME_RUNTIME_DIR = join(root, "run")
mkdirSync(process.env.RHIZOME_RUNTIME_DIR, { mode: 0o700 })
const { default: extension, socketPath } = await import("./rhizome.js")

// A fake Pi: records handlers, MCP registrations and delivered messages.
function fakePi() {
  const handlers = {}
  const pi = {
    handlers, servers: [], sent: [],
    on: (name, fn) => { handlers[name] = fn },
    registerMcpServer: (name, spec) => pi.servers.push({ name, spec }),
    unregisterMcpServer: () => {},
    sendMessage: (msg, opts) => pi.sent.push({ msg, opts }),
  }
  extension(pi)
  return pi
}

const ctx = (id) => ({ cwd: root, sessionManager: { getSessionId: () => id } })

// Accepts the extension's connection and collects its frames.
function listen(path) {
  const frames = []
  let socket = null
  let wake = null
  const server = createServer((s) => {
    socket = s
    let buf = ""
    s.setEncoding("utf8")
    s.on("data", (d) => {
      buf += d
      let i
      while ((i = buf.indexOf("\n")) >= 0) { frames.push(JSON.parse(buf.slice(0, i))); buf = buf.slice(i + 1) }
      wake?.()
    })
  })
  server.listen(path)
  return {
    frames, send: (f) => socket.write(JSON.stringify(f) + "\n"), close: () => { socket?.destroy(); server.close() },
    until: async (pred) => {
      const deadline = Date.now() + 5000
      while (!pred(frames)) {
        if (Date.now() > deadline) assert.fail("timed out; frames: " + JSON.stringify(frames))
        await new Promise((r) => { wake = r; setTimeout(r, 25) })
      }
    },
  }
}

const start = async (pi, id) => {
  await pi.handlers.session_start({}, ctx(id))
  const spec = pi.servers.at(-1).spec
  return { spec, path: socketPath(process.env.RHIZOME_RUNTIME_DIR, process.pid, spec.env.RHIZOME_PI_NONCE) }
}

test("registers once per session, with a fresh nonce, and ignores a duplicate session_start", async () => {
  const pi = fakePi()
  const a = await start(pi, "s1")
  await pi.handlers.session_start({}, ctx("s1"))
  assert.equal(pi.servers.length, 1)
  assert.deepEqual(a.spec.args.slice(0, 3), ["mcp", "--agent", "pi"])
  assert.match(a.spec.env.RHIZOME_PI_NONCE, /^[0-9a-f]{32}$/)
  const b = await start(pi, "s2")
  assert.notEqual(a.spec.env.RHIZOME_PI_NONCE, b.spec.env.RHIZOME_PI_NONCE)
  await pi.handlers.session_shutdown({})
})

test("queues events before the bind, then relays them in order after it", async () => {
  const pi = fakePi()
  const { path } = await start(pi, "s1")
  await pi.handlers.message_end({ message: { role: "user", content: "fix it" } })
  await pi.handlers.message_end({ message: { role: "assistant", responseId: "r1", content: [{ type: "thinking", thinking: "hmm" }, { type: "text", text: "on it" }, { type: "toolCall", id: "t", name: "read", arguments: {} }] } })
  await pi.handlers.tool_execution_start({ toolCallId: "c1", toolName: "read", args: { path: "a" } })
  await pi.handlers.tool_execution_start({ toolCallId: "c1/1", toolName: "nested", args: {}, parentToolCallId: "c1" })
  await pi.handlers.tool_execution_end({ toolCallId: "c1", toolName: "read", isError: true, result: { content: [{ type: "text", text: "boom" }] } })
  await pi.handlers.agent_end({ messages: [] })
  const srv = listen(path)
  await srv.until((f) => f.some((x) => x.type === "turn"))
  assert.deepEqual(srv.frames.map((f) => f.type), ["hello", "bind", "input", "text", "tool", "tool", "turn"])
  assert.equal(srv.frames[1].sessionID, "s1")
  assert.deepEqual(srv.frames.slice(2).map(({ sessionID, ...f }) => f), [
    { type: "input", kind: "user", text: "fix it" },
    { type: "text", messageID: "r1", text: "on it" },
    { type: "tool", callID: "c1", name: "read", phase: "start", text: '{"path":"a"}' },
    { type: "tool", callID: "c1", name: "read", phase: "failed", text: "boom" },
    { type: "turn" },
  ])
  srv.close()
  await pi.handlers.session_shutdown({})
})

test("a full queue drops the oldest events and reports the overflow before them", async () => {
  const pi = fakePi()
  const { path } = await start(pi, "s1")
  for (let i = 0; i < 300; i++) await pi.handlers.message_end({ message: { role: "assistant", responseId: `r${i}`, content: [{ type: "text", text: `t${i}` }] } })
  const srv = listen(path)
  await srv.until((f) => f.filter((x) => x.type === "text").length >= 256)
  const [, , overflow, first] = srv.frames
  assert.equal(overflow.type, "overflow")
  assert.equal(overflow.dropped, 44)
  assert.equal(first.text, "t44")
  srv.close()
  await pi.handlers.session_shutdown({})
})

test("a delivery becomes a custom message, marks the next turn as a peer turn and is acknowledged", async () => {
  const pi = fakePi()
  const { path } = await start(pi, "s1")
  const srv = listen(path)
  await srv.until((f) => f.some((x) => x.type === "bind"))
  await new Promise((r) => setTimeout(r, 50))
  srv.send({ type: "deliver", id: "d1", sessionID: "s1", text: "<rhizome-peer-event>hi</rhizome-peer-event>" })
  await srv.until((f) => f.some((x) => x.type === "ack"))
  assert.deepEqual(pi.sent[0].opts, { deliverAs: "followUp", triggerTurn: true })
  assert.equal(pi.sent[0].msg.customType, "rhizome")
  // Queued is not delivered: no marker until Pi feeds the message to the agent.
  assert.equal(srv.frames.some((f) => f.type === "input"), false)
  await pi.handlers.message_end({ message: { role: "custom", customType: "rhizome", content: "x" } })
  await srv.until((f) => f.some((x) => x.type === "input" && x.rhizome === true))
  // A delivery for another session is refused, never executed.
  srv.send({ type: "deliver", id: "d2", sessionID: "other", text: "x" })
  await srv.until((f) => f.some((x) => x.type === "fail" && x.id === "d2"))
  assert.equal(pi.sent.length, 1)
  srv.close()
  await pi.handlers.session_shutdown({})
})

test("an oversized text is cut to fit one frame", async () => {
  const pi = fakePi()
  const { path } = await start(pi, "s1")
  await pi.handlers.message_end({ message: { role: "assistant", responseId: "big", content: [{ type: "text", text: '"\n'.repeat(60000) }] } })
  const srv = listen(path)
  await srv.until((f) => f.some((x) => x.type === "text"))
  assert.ok(JSON.stringify(srv.frames.find((f) => f.type === "text")).length < 64 * 1024)
  srv.close()
  await pi.handlers.session_shutdown({})
})

test("peerView unwraps the envelope for display", async () => {
  const { peerView } = await import("./rhizome.js")
  const env = 'Rhizome peer activity.\n<rhizome-peer-event>\n{"content":"[activity] Sent: “Hi”","origin":{"session_id":"abcdef123","kind":"activity","sequence":"3"}}\n</rhizome-peer-event>'
  const v = peerView(env)
  assert.equal(v.body, "[activity] Sent: “Hi”")
  assert.equal(v.origin.kind, "activity")
  assert.equal(peerView("plain").body, "plain")
})

test("the rhizome renderer is registered and draws an arrow line", () => {
  let renderer
  const pi = { on() {}, registerMcpServer() {}, unregisterMcpServer() {}, sendMessage() {},
    registerMessageRenderer: (t, fn) => { if (t === "rhizome") renderer = fn } }
  extension(pi)
  const theme = { fg: (_c, t) => t }
  const c = renderer({ content: '<rhizome-peer-event>{"content":"[direct] hi","origin":{}}</rhizome-peer-event>' }, { expanded: false }, theme)
  assert.deepEqual(c.render(80), ["", "← rhizome: [direct] hi"])
})

test("the renderer sanitizes peer text and never exceeds the width", async () => {
  const { cellWidth } = await import("./rhizome.js")
  let renderer
  const pi = { on() {}, registerMcpServer() {}, unregisterMcpServer() {}, sendMessage() {},
    registerMessageRenderer: (t, fn) => { if (t === "rhizome") renderer = fn } }
  extension(pi)
  const theme = { fg: (_c, t) => t }
  const env = (body, origin = {}) => "<rhizome-peer-event>" + JSON.stringify({ content: body, origin }) + "</rhizome-peer-event>"
  const cols = (l) => [...l].reduce((n, ch) => n + cellWidth(ch), 0)
  for (const width of [80, 20, 8, 3]) {
    for (const body of ["界".repeat(50), "😀 ok ".repeat(30), "plain ".repeat(40)]) {
      const c = renderer({ content: env(body, { session_id: "abcdef1234", kind: "k", sequence: "1" }) }, { expanded: true }, theme)
      for (const l of c.render(width)) assert.ok(cols(l) <= width, `width ${width}: ${cols(l)} in ${JSON.stringify(l)}`)
    }
  }
  const c = renderer({ content: env("\u001b[2J\u001b[31mspoof\rhidden\u001b]0;t\u0007x") }, {}, theme)
  const out = c.render(80).join("\n")
  assert.ok(!/[\u0000-\u0009\u000b-\u001f\u007f]/.test(out), JSON.stringify(out))
  assert.ok(out.includes("spoof"))
})
