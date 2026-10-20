import { test } from "node:test"
import assert from "node:assert/strict"
import { createServer } from "node:net"
import { createHash } from "node:crypto"
import { mkdtempSync, mkdirSync, realpathSync, rmSync } from "node:fs"
import { join } from "node:path"

const root = mkdtempSync("/tmp/rzp-")
process.env.RHIZOME_RUNTIME_DIR = join(root, "run")
mkdirSync(process.env.RHIZOME_RUNTIME_DIR, { mode: 0o700 })
const project = join(root, "project")
mkdirSync(project)
const sock = join(process.env.RHIZOME_RUNTIME_DIR, `opencode-${process.pid}-${createHash("sha256").update(realpathSync(project)).digest("hex").slice(0, 12)}.sock`)
const plugin = (await import("./rhizome.js")).default

function fakeCtx({ getResult = { parentID: "", title: "Fix login", location: { directory: project } }, synthetic } = {}) {
  const hooks = {}
  const events = []
  let wake = null
  const calls = { synthetic: [] }
  const ctx = {
    app: { version: "2.0.32" },
    location: { directory: project },
    tool: { hook: (name, fn) => { hooks[name] = fn; return { dispose() {} } } },
    session: {
      get: async ({ sessionID }) => (typeof getResult === "function" ? getResult(sessionID) : getResult),
      synthetic: synthetic ?? (async (args) => { calls.synthetic.push(args); return { id: "msg_1" } }),
    },
    event: {
      subscribe: ({ signal }) => ({
        [Symbol.asyncIterator]() {
          return {
            next: () => new Promise((resolve) => {
              const pull = () => events.length ? resolve({ value: events.shift(), done: false }) : signal.aborted ? resolve({ done: true }) : (wake = pull)
              signal.addEventListener("abort", () => { wake = null; resolve({ done: true }) }, { once: true })
              pull()
            }),
          }
        },
      }),
    },
  }
  const emit = (event) => { events.push(event); const w = wake; wake = null; w?.() }
  return { ctx, hooks, emit, calls }
}

function listen() {
  const lines = []
  const all = []
  let waiter = null
  let conn = null
  const server = createServer((c) => {
    conn = c
    let buf = ""
    c.setEncoding("utf8")
    c.on("data", (d) => {
      buf += d
      let i
      while ((i = buf.indexOf("\n")) >= 0) {
        const frame = JSON.parse(buf.slice(0, i)); buf = buf.slice(i + 1)
        lines.push(frame); all.push(frame)
        const w = waiter; waiter = null; w?.()
      }
    })
  })
  const next = (pred = () => true) => new Promise((resolve, reject) => {
    const deadline = setTimeout(() => reject(new Error("frame timeout")), 5000)
    const check = () => {
      const i = lines.findIndex(pred)
      if (i >= 0) { clearTimeout(deadline); resolve(lines.splice(i, 1)[0]) } else waiter = check
    }
    check()
  })
  return {
    start: () => new Promise((r) => server.listen(sock, r)),
    next, lines, all,
    write: (f) => conn.write(JSON.stringify(f) + "\n"),
    stop: () => new Promise((r) => { conn?.destroy(); server.close(r) }),
  }
}

test("hello, binding, inputs, text, tools, turn and title", async () => {
  const srv = listen(); await srv.start()
  const { ctx, hooks, emit } = fakeCtx()
  const cleanup = plugin.setup(ctx)
  assert.equal((await srv.next()).type, "hello")
  emit({ type: "session.inbox.enqueued", data: { sessionID: "ses_a", item: { type: "user", payload: { text: "fix it" } } } })
  assert.deepEqual(await srv.next(), { type: "bind", sessionID: "ses_a", parentID: "", directory: project, title: "Fix login" })
  assert.deepEqual(await srv.next(), { type: "input", sessionID: "ses_a", kind: "user", rhizome: false })
  emit({ type: "session.inbox.enqueued", data: { sessionID: "ses_a", item: { type: "synthetic", payload: { text: "x <rhizome-peer-event>{}</rhizome-peer-event>" } } } })
  assert.deepEqual(await srv.next(), { type: "input", sessionID: "ses_a", kind: "synthetic", rhizome: true })
  emit({ type: "session.text.ended", data: { sessionID: "ses_a", assistantMessageID: "msg_1", text: "é".repeat(10000) } })
  const text = await srv.next()
  assert.equal(text.type, "text"); assert.ok(Buffer.byteLength(text.text) <= 16 * 1024); assert.ok(!text.text.includes("�"))
  await hooks["execute.before"]({ tool: "read", sessionID: "ses_a", id: "c1", input: { path: "/secret" } })
  await hooks["execute.after"]({ tool: "read", sessionID: "ses_a", id: "c1", status: "completed", result: "file body" })
  assert.deepEqual(await srv.next(), { type: "tool", sessionID: "ses_a", callID: "c1", name: "read", phase: "start" })
  assert.deepEqual(await srv.next(), { type: "tool", sessionID: "ses_a", callID: "c1", name: "read", phase: "completed" })
  emit({ type: "session.execution.succeeded", data: { sessionID: "ses_a" } })
  assert.deepEqual(await srv.next(), { type: "turn", sessionID: "ses_a" })
  emit({ type: "session.renamed", data: { sessionID: "ses_a", title: "New" } })
  assert.deepEqual(await srv.next(), { type: "title", sessionID: "ses_a", title: "New" })
  emit({ type: "session.deleted", data: { sessionID: "ses_a" } })
  assert.deepEqual(await srv.next(), { type: "forget", sessionID: "ses_a" })
  assert.ok(!JSON.stringify(srv.all).includes("file body") && !JSON.stringify(srv.all).includes("/secret"))
  cleanup(); await srv.stop()
})

test("deliveries are executed as queued synthetic messages and acknowledged", async () => {
  const srv = listen(); await srv.start()
  const ok = fakeCtx()
  const cleanup = plugin.setup(ok.ctx)
  await srv.next((f) => f.type === "hello")
  srv.write({ type: "deliver", id: "d1", sessionID: "ses_a", text: "peer" })
  assert.deepEqual(await srv.next((f) => f.type === "ack"), { type: "ack", id: "d1" })
  assert.deepEqual(ok.calls.synthetic[0], { sessionID: "ses_a", text: "peer", delivery: "queue" })
  cleanup(); await srv.stop()
  const srv2 = listen(); await srv2.start()
  const bad = fakeCtx({ synthetic: async () => { throw new Error("session gone") } })
  const cleanup2 = plugin.setup(bad.ctx)
  await srv2.next((f) => f.type === "hello")
  srv2.write({ type: "deliver", id: "d2", sessionID: "ses_a", text: "peer" })
  const fail = await srv2.next((f) => f.type === "fail")
  assert.equal(fail.id, "d2"); assert.match(fail.reason, /session gone/)
  cleanup2(); await srv2.stop()
})

test("child sessions bind with their parent so Rhizome can refuse them", async () => {
  const srv = listen(); await srv.start()
  const { ctx, emit } = fakeCtx({ getResult: { parentID: "ses_parent", title: "child", location: { directory: project } } })
  const cleanup = plugin.setup(ctx)
  await srv.next((f) => f.type === "hello")
  emit({ type: "session.inbox.enqueued", data: { sessionID: "ses_c", item: { type: "user", payload: { text: "go" } } } })
  assert.equal((await srv.next((f) => f.type === "bind")).parentID, "ses_parent")
  cleanup(); await srv.stop()
})

test("sessions from another location are never bound or forwarded", async () => {
  const other = join(root, "other")
  mkdirSync(other)
  const srv = listen(); await srv.start()
  const { ctx, hooks, emit } = fakeCtx({
    getResult: (id) => id === "ses_mine"
      ? { parentID: "", title: "mine", location: { directory: project } }
      : id === "ses_nowhere" ? { parentID: "", title: "none" }
      : id === "ses_gone" ? { parentID: "", title: "gone", location: { directory: join(root, "missing") } }
      : { parentID: "", title: "theirs", location: { directory: other } },
  })
  const cleanup = plugin.setup(ctx)
  await srv.next((f) => f.type === "hello")
  for (const id of ["ses_other", "ses_nowhere", "ses_gone"]) {
    emit({ type: "session.inbox.enqueued", data: { sessionID: id, item: { type: "user", payload: { text: "go" } } } })
    emit({ type: "session.text.ended", data: { sessionID: id, assistantMessageID: "m", text: "hi" } })
    emit({ type: "session.execution.succeeded", data: { sessionID: id } })
    emit({ type: "session.renamed", data: { sessionID: id, title: "t" } })
    emit({ type: "session.deleted", data: { sessionID: id } })
    await hooks["execute.before"]({ tool: "read", sessionID: id, id: "c1" })
  }
  // A local session afterwards proves the earlier events were processed and dropped.
  emit({ type: "session.inbox.enqueued", data: { sessionID: "ses_mine", item: { type: "user", payload: { text: "go" } } } })
  assert.deepEqual(await srv.next(), { type: "bind", sessionID: "ses_mine", parentID: "", directory: project, title: "mine" })
  assert.deepEqual(await srv.next(), { type: "input", sessionID: "ses_mine", kind: "user", rhizome: false })
  assert.deepEqual(srv.all.filter((f) => f.type !== "hello").map((f) => f.sessionID), ["ses_mine", "ses_mine"])
  cleanup(); await srv.stop()
})

test("buffer is bounded before connect and reports overflow first", async () => {
  const { ctx, emit } = fakeCtx()
  const cleanup = plugin.setup(ctx)
  emit({ type: "session.inbox.enqueued", data: { sessionID: "ses_a", item: { type: "user", payload: { text: "go" } } } })
  for (let i = 0; i < 300; i++) emit({ type: "session.text.ended", data: { sessionID: "ses_a", assistantMessageID: `m${i}`, text: "t" } })
  await new Promise((r) => setTimeout(r, 50))
  const srv = listen(); await srv.start()
  assert.equal((await srv.next()).type, "hello")
  // 302 frames (bind, input, 300 text) into a 256-frame buffer: 46 dropped.
  const overflow = await srv.next()
  assert.deepEqual(overflow, { type: "overflow", dropped: 46 })
  const first = await srv.next()
  assert.deepEqual(first, { type: "text", sessionID: "ses_a", messageID: "m44", text: "t" })
  cleanup(); await srv.stop()
})

test.after(() => rmSync(root, { recursive: true, force: true }))
