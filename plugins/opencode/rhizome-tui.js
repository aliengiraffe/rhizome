// Rhizome TUI plugin for OpenCode 2.x. OpenCode hides synthetic input in the
// TUI, so a Rhizome peer delivery reaches the model but not the human. This
// plugin only displays: when a synthetic inbox item carrying Rhizome's peer
// envelope is enqueued for a session of the TUI's directory, it shows a toast
// with a sanitized, truncated excerpt. It makes no Rhizome decision, opens no
// socket or file, sends nothing anywhere and logs nothing.

const PEER_MARKER = "<rhizome-peer-event>"
const PEER_END = "</rhizome-peer-event>"
const MAX_ENVELOPE = 64 * 1024
const MAX_MESSAGE = 200
const MAX_KIND = 32
const MAX_SEEN = 256

// Control characters (including ANSI escapes and newlines) and bidirectional
// or zero-width formatting characters become spaces; peer text is untrusted
// and must render as plain, single-line text.
const UNSAFE = /[\p{Cc}\u200b-\u200f\u2028-\u202e\u2060-\u2069\ufeff]/gu

function plain(text, max) {
  const flat = String(text ?? "").replace(UNSAFE, " ").replace(/\s+/g, " ").trim()
  const chars = Array.from(flat)
  if (chars.length <= max) return flat
  return chars.slice(0, max - 1).join("").trimEnd() + "\u2026"
}

// Parse Rhizome's peer envelope (internal/channel/delivery.go peerEnvelope):
// a JSON object {origin: {kind, session_id, ...}, content} between the markers.
// Anything else is not a Rhizome delivery and shows nothing.
export function parseEnvelope(text) {
  if (typeof text !== "string" || text.length > MAX_ENVELOPE) return null
  const start = text.indexOf(PEER_MARKER)
  if (start < 0) return null
  const end = text.indexOf(PEER_END, start)
  if (end < 0) return null
  let body
  try { body = JSON.parse(text.slice(start + PEER_MARKER.length, end)) } catch { return null }
  if (!body || typeof body !== "object" || typeof body.content !== "string") return null
  const origin = body.origin && typeof body.origin === "object" ? body.origin : {}
  return {
    kind: typeof origin.kind === "string" ? origin.kind : "",
    sessionID: typeof origin.session_id === "string" ? origin.session_id : "",
    content: body.content,
  }
}

// Build the toast for an envelope. The title names the event kind and the
// sender's short session ID; the message is the content without its leading
// "[kind]" tag, which the title already shows.
export function toastFor(envelope) {
  const kind = plain(envelope.kind, MAX_KIND) || "peer event"
  const sender = plain(envelope.sessionID, 64).replace(/\s/g, "").slice(0, 8)
  let content = envelope.content
  const tag = `[${envelope.kind}]`
  if (envelope.kind !== "" && content.startsWith(tag)) content = content.slice(tag.length)
  return {
    title: sender ? `Rhizome \u00b7 ${kind} from ${sender}` : `Rhizome \u00b7 ${kind}`,
    message: plain(content, MAX_MESSAGE) || "(empty)",
    variant: "info",
  }
}

export default {
  id: "rhizome.tui",
  setup(ctx) {
    const seen = new Set()
    const here = () => ctx.location?.directory ?? ctx.data?.location?.default?.()?.directory
    const onEnqueued = (event) => {
      try {
        const d = event?.data
        const item = d?.item
        if (item?.type !== "synthetic") return
        const sessionID = d?.sessionID
        if (typeof sessionID !== "string" || sessionID === "") return
        const envelope = parseEnvelope(item?.payload?.text)
        if (!envelope) return
        // Only sessions of this TUI's directory; unknown sessions show nothing.
        const dir = ctx.data?.session?.get?.(sessionID)?.location?.directory
        const current = here()
        if (!dir || !current || dir !== current) return
        const inboxID = d?.inboxID
        if (typeof inboxID === "string" && inboxID !== "") {
          if (seen.has(inboxID)) return
          if (seen.size >= MAX_SEEN) seen.delete(seen.values().next().value)
          seen.add(inboxID)
        }
        ctx.ui.toast.show({ ...toastFor(envelope), sessionID })
      } catch {}
    }
    let off
    try { off = ctx.data.on("session.inbox.enqueued", onEnqueued) } catch {}
    return () => {
      try { if (typeof off === "function") off() } catch {}
      seen.clear()
    }
  },
}
