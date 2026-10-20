// Rhizome plugin for OpenCode 2.x. It relays a fixed allowlist of session
// events to the local Rhizome MCP process over a private Unix socket and
// executes Rhizome's deliveries as queued synthetic messages. Every filtering,
// identity and routing decision lives in Rhizome; this plugin never decides.
import { createConnection } from "node:net"
import { createHash } from "node:crypto"
import { realpathSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"

const PROTOCOL = 1
const MAX_BUFFER = 256
const MAX_TEXT = 16 * 1024
const MAX_LINE = 64 * 1024
const MAX_PENDING_WRITE = 1024 * 1024
const PEER_MARKER = "<rhizome-peer-event>"

// Must match bridgeSocketPath in internal/channel/opencode_bridge.go.
function socketPath(directory) {
  const runtime = process.env.RHIZOME_RUNTIME_DIR || join(homedir(), ".rhizome", "run")
  const hash = createHash("sha256").update(realpathSync(directory)).digest("hex").slice(0, 12)
  return join(runtime, `opencode-${process.pid}-${hash}.sock`)
}

// Cut UTF-8 text to MAX_TEXT bytes without splitting a character.
function truncate(text) {
  const buf = Buffer.from(String(text ?? ""), "utf8")
  if (buf.length <= MAX_TEXT) return buf.toString("utf8")
  let end = MAX_TEXT
  while (end > 0 && (buf[end] & 0xc0) === 0x80) end--
  return buf.subarray(0, end).toString("utf8")
}

export default {
  id: "rhizome",
  setup(ctx) {
    const directory = ctx.location.directory
    let realDirectory = null
    try { realDirectory = realpathSync(directory) } catch {}
    // Session IDs this instance has bound as top-level sessions. OpenCode
    // delivers every session event to every location's plugin instance, so
    // only these sessions are relayed.
    const local = new Set()
    const path = socketPath(directory)
    const queue = []
    let dropped = 0
    let socket = null
    let ready = false
    let closed = false
    let backoff = 250
    let timer = null

    const line = (frame) => JSON.stringify(frame) + "\n"
    const flush = () => {
      if (!ready) return
      if (dropped > 0) { socket.write(line({ type: "overflow", dropped })); dropped = 0 }
      while (queue.length > 0 && socket.writableLength < MAX_PENDING_WRITE) socket.write(line(queue.shift()))
    }
    const send = (frame) => {
      if (queue.length >= MAX_BUFFER) { queue.shift(); dropped++ }
      queue.push(frame)
      flush()
    }
    const reply = (frame) => { if (ready && socket) socket.write(line(frame)) }

    const deliver = async (frame) => {
      try {
        await ctx.session.synthetic({ sessionID: frame.sessionID, text: frame.text, delivery: "queue" })
        reply({ type: "ack", id: frame.id })
      } catch (error) {
        reply({ type: "fail", id: frame.id, reason: String(error?.message ?? error).slice(0, 200) })
      }
    }

    const connect = () => {
      if (closed) return
      const s = createConnection(path)
      socket = s
      let buffer = ""
      s.setEncoding("utf8")
      s.on("connect", () => {
        backoff = 250
        s.write(line({ type: "hello", version: PROTOCOL, opencode: String(ctx.app?.version ?? "") }))
        ready = true
        flush()
      })
      s.on("drain", flush)
      s.on("data", (chunk) => {
        buffer += chunk
        let i
        while ((i = buffer.indexOf("\n")) >= 0) {
          const raw = buffer.slice(0, i)
          buffer = buffer.slice(i + 1)
          let frame
          try { frame = JSON.parse(raw) } catch { continue }
          if (frame?.type === "deliver") void deliver(frame)
        }
        if (buffer.length > MAX_LINE) s.destroy()
      })
      s.on("error", () => {})
      s.on("close", () => {
        if (socket === s) { socket = null; ready = false }
        if (closed) return
        timer = setTimeout(connect, backoff)
        timer.unref?.()
        backoff = Math.min(backoff * 2, 15000)
      })
    }

    // True only when the session's location directory is this instance's.
    const inThisLocation = (info) => {
      const dir = info?.location?.directory
      if (!dir || !realDirectory) return false
      try { return realpathSync(dir) === realDirectory } catch { return false }
    }

    // Bind only sessions of this location that a human prompted. Children are
    // sent so Rhizome can refuse them using parentID but are never relayed.
    // Unverifiable or foreign sessions never bind.
    const bind = async (sessionID) => {
      let info
      try { info = await ctx.session.get({ sessionID }) } catch { return false }
      if (!inThisLocation(info)) return false
      const parentID = String(info?.parentID ?? "")
      send({ type: "bind", sessionID, parentID, directory, title: String(info?.title ?? "") })
      if (parentID === "") local.add(sessionID)
      return true
    }

    const onEvent = async (event) => {
      const d = event?.data ?? {}
      switch (event?.type) {
        case "session.inbox.enqueued": {
          const kind = d.item?.type
          if (kind !== "user" && kind !== "synthetic") return
          const rhizome = String(d.item?.payload?.text ?? "").includes(PEER_MARKER)
          if (kind === "user" && !rhizome) await bind(d.sessionID)
          if (!local.has(d.sessionID)) return
          send({ type: "input", sessionID: d.sessionID, kind, rhizome })
          return
        }
        case "session.text.ended":
          if (!local.has(d.sessionID)) return
          send({ type: "text", sessionID: d.sessionID, messageID: String(d.assistantMessageID ?? ""), text: truncate(d.text) })
          return
        case "session.execution.succeeded":
          if (!local.has(d.sessionID)) return
          send({ type: "turn", sessionID: d.sessionID })
          return
        case "session.renamed":
          if (!local.has(d.sessionID)) return
          send({ type: "title", sessionID: d.sessionID, title: String(d.title ?? "") })
          return
        case "session.deleted":
          if (!local.has(d.sessionID)) return
          send({ type: "forget", sessionID: d.sessionID })
          local.delete(d.sessionID)
          return
      }
    }

    const tool = (phase) => async (ev) => {
      if (!local.has(ev.sessionID)) return
      send({ type: "tool", sessionID: ev.sessionID, callID: String(ev.id ?? ""), name: String(ev.tool ?? ""), phase: phase(ev) })
    }
    const registrations = [
      ctx.tool.hook("execute.before", tool(() => "start")),
      ctx.tool.hook("execute.after", tool((ev) => (ev.status === "completed" ? "completed" : "failed"))),
    ]
    const controller = new AbortController()
    void (async () => {
      try {
        for await (const event of ctx.event.subscribe({ signal: controller.signal })) await onEvent(event)
      } catch {}
    })()
    connect()

    return () => {
      closed = true
      controller.abort()
      clearTimeout(timer)
      for (const r of registrations) r?.dispose?.()
      socket?.destroy()
    }
  },
}
