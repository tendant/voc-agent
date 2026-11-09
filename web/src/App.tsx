import { useCallback, useEffect, useRef, useState } from 'react'
import { Mic, Player } from './audio'
import { ApprovalDialog, type ApprovalRequest } from './Approval'
import { AuditLog } from './Audit'

type Line = {
  key: string
  who: 'you' | 'assistant' | 'system' | 'tool' | 'reminder'
  text: string
  meta?: string
  partial?: boolean
  ok?: boolean
}
type Reminder = { id: number; text: string; due_at: string; status: string; source?: string }
type Status = 'connecting' | 'ready' | 'listening' | 'thinking' | 'speaking' | 'offline'
type ServerEvent = {
  type: string
  turn?: number
  text?: string
  lang?: string
  final?: boolean
  ms?: Record<string, number>
  error?: string
  name?: string
  args?: unknown
  result?: unknown
  ok?: boolean
}

const NOTIFY_TURN = 0xffffffff

/** One-line human summary of a tool call and its result for the transcript. */
function describeTool(name: string, args: any, result: any, ok?: boolean): string {
  if (ok === false) return `${name} failed: ${result?.error ?? 'error'}`
  switch (name) {
    case 'search_notes': {
      const hits = (result?.results ?? []) as { title: string }[]
      return `Searched notes for “${args?.query}”: ${hits.length ? hits.map((h) => h.title).join(', ') : 'no matches'}`
    }
    case 'create_reminder':
      return `Reminder ${result?.status === 'already_exists' ? 'already existed' : 'created'}: “${result?.reminder?.text}”, ${result?.reminder?.due}`
    case 'list_reminders':
      return `Listed ${result?.count ?? 0} upcoming reminder(s)`
    case 'youtube_play':
      return `Playing “${result?.title}” on YouTube`
    case 'media':
      return `${result?.action ?? args?.action}: ${result?.playing ? 'playing' : 'paused'}${result?.ad ? ' (an ad is showing)' : ''}${result?.title ? ` — ${result.title}` : ''}`
    case 'page_read':
      return `Read the page “${result?.untrusted_page_content?.title ?? ''}”`
    case 'page_click':
      return `Clicked “${result?.label}”`
    case 'page_nav':
      return { back: 'Went back', forward: 'Went forward', reload: 'Reloaded', scroll_down: 'Scrolled down', scroll_up: 'Scrolled up', top: 'Top of the page', bottom: 'Bottom of the page' }[result?.action as string] ?? `Page: ${result?.action}`
    case 'page_type':
      return `Typed into ${result?.field || 'the page'}${result?.submitted ? ' and pressed Enter' : ''}`
    case 'page_result':
      return `Opened result ${result?.n}: “${result?.title}”`
    case 'web_search':
      return `Opened ${result?.site} search for “${result?.query}”`
    case 'open_url':
      return `Opened ${result?.url}`
    case 'open_app':
      return `Opened ${result?.app}`
    case 'ask_cloud':
      if (result?.status === 'answered') return `Cloud answered (${result?.model})`
      if (result?.status === 'blocked') return 'Blocked by privacy policy — nothing was sent'
      return 'Cloud request not sent'
  }
  return `${name} ${JSON.stringify(args)}`
}

/** Space means push-to-talk unless it has another job where focus is: typing
 * in a field, or pressing some other button. Firefox keeps focus on a clicked
 * button, so checking for document.body alone misses the common case. */
function spaceIsPushToTalk(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null
  if (!el || el === document.body || el === document.documentElement) return true
  if (el.isContentEditable || el.closest('input, textarea, select, [contenteditable]')) return false
  const button = el.closest('button, a, summary, [role="button"]')
  return !button || button.hasAttribute('data-ptt')
}

function fmtDue(iso: string) {
  const d = new Date(iso)
  return d.toLocaleString(undefined, { weekday: 'short', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })
}

const STAGES = ['asr', 'llm_ttft', 'tts_first', 'heard', 'tools', 'llm_done', 'total'] as const

export default function App() {
  const ws = useRef<WebSocket | null>(null)
  const mic = useRef(new Mic())
  const player = useRef<Player | null>(null)
  const [status, setStatus] = useState<Status>('connecting')
  const [lines, setLines] = useState<Line[]>([])
  const [timing, setTiming] = useState<Record<string, number>>({})
  const [draft, setDraft] = useState('')
  const busy = useRef(false) // a turn is in flight
  const playing = useRef(false)
  const turn = useRef(0) // latest turn whose output we accept; 0 = none
  const sentAt = useRef(0) // performance.now() when the user finished
  const heard = useRef(false)
  const nextKey = useRef(0)
  const scroller = useRef<HTMLElement>(null)
  const [reminders, setReminders] = useState<Reminder[]>([])
  const pendingArgs = useRef<Record<string, unknown>>({})
  const [approval, setApproval] = useState<ApprovalRequest | null>(null)
  const approvalRef = useRef<ApprovalRequest | null>(null)
  approvalRef.current = approval
  const [auditTick, setAuditTick] = useState(0)
  const [cloud, setCloud] = useState<{ enabled: boolean; model?: string; destination?: string; why?: string } | null>(null)
  // Hands-free: the mic streams continuously and the server finds utterances.
  const [handsFree, setHandsFree] = useState(false)
  const handsFreeRef = useRef(false)
  handsFreeRef.current = handsFree
  const [hearing, setHearing] = useState(false)
  // Hands-free falls asleep after a quiet spell; then only the wake word works.
  const [asleep, setAsleep] = useState<string | null>(null) // wake word while asleep
  const unmuteTimer = useRef<number | undefined>(undefined)

  const loadReminders = useCallback(() => {
    fetch('/api/reminders')
      .then((r) => (r.ok ? r.json() : []))
      .then(setReminders)
      .catch(() => {})
  }, [])

  const add = (l: Omit<Line, 'key'> & { key?: string }) =>
    setLines((ls) => [...ls, { key: `l${nextKey.current++}`, ...l }])
  const upsert = (key: string, f: (l: Line | undefined) => Line) =>
    setLines((ls) => {
      const i = ls.findIndex((l) => l.key === key)
      if (i < 0) return [...ls, f(undefined)]
      const copy = ls.slice()
      copy[i] = f(ls[i])
      return copy
    })

  const settle = () => setStatus(playing.current ? 'speaking' : busy.current ? 'thinking' : 'ready')

  const getPlayer = () => {
    if (!player.current) {
      player.current = new Player() // created on a user gesture so autoplay is allowed
      player.current.onPlaying = (p) => {
        playing.current = p
        // Half-duplex: don't stream the mic while voc is talking (plus a short
        // tail for room echo), or hands-free mode would answer itself.
        window.clearTimeout(unmuteTimer.current)
        if (p) mic.current.muted = true
        else unmuteTimer.current = window.setTimeout(() => (mic.current.muted = false), 350)
        setStatus((s) => (s === 'listening' ? s : p ? 'speaking' : busy.current ? 'thinking' : 'ready'))
      }
    }
    return player.current
  }

  useEffect(() => {
    let closed = false
    let retry: number | undefined
    const onEvent = (m: ServerEvent) => {
      const t = m.turn ?? 0
      switch (m.type) {
        case 'turn_start':
          if (t !== turn.current) player.current?.stop() // a new turn replaces the old one's audio
          turn.current = t
          heard.current = false
          setTiming({})
          if (handsFreeRef.current) {
            busy.current = true
            setStatus((s) => (s === 'listening' ? s : 'thinking'))
          }
          return
        case 'speech':
          setHearing(!!m.ok)
          if (!m.ok) sentAt.current = performance.now() // end of speech: start the clock
          return
        case 'ignored':
          return
        case 'listen_state':
          setAsleep(m.text === 'asleep' ? (m.name ?? 'Victor') : null)
          return
        case 'listen_stopped':
          setHandsFree(false)
          setHearing(false)
          setAsleep(null)
          mic.current.stop()
          add({ who: 'system', text: 'Hands-free listening is off.' })
          return
        case 'error':
          busy.current = false
          settle()
          add({ who: 'system', text: m.error ?? 'error' })
          return
        case 'reminder_fired':
          add({ who: 'reminder', text: `Reminder: ${m.text}` })
          getPlayer() // make sure audio can play
          loadReminders()
          return
      }
      if (t !== turn.current) return // output from an abandoned turn
      switch (m.type) {
        case 'transcript':
          add({ who: 'you', text: m.text || '(no speech detected)', meta: m.lang })
          break
        case 'llm_delta':
          upsert(`a${t}`, (l) => ({ key: `a${t}`, who: 'assistant', text: (l?.text ?? '') + m.text, partial: true }))
          break
        case 'assistant_done':
          busy.current = false
          if (m.text) upsert(`a${t}`, () => ({ key: `a${t}`, who: 'assistant', text: m.text!, partial: !m.final }))
          settle()
          break
        case 'timing':
          setTiming((x) => ({ ...x, ...m.ms }))
          break
        case 'recall': {
          const notes = (m.result ?? []) as { title: string }[]
          add({ who: 'tool', text: `Looked in notes: ${notes.map((n) => n.title).join(', ')}` })
          break
        }
        case 'tool_call':
          pendingArgs.current[m.name!] = m.args
          break
        case 'tool_result':
          add({ who: 'tool', text: describeTool(m.name!, pendingArgs.current[m.name!], m.result, m.ok), ok: m.ok, meta: `${m.ms?.ms ?? 0} ms` })
          if (m.name?.includes('reminder')) loadReminders()
          break
        case 'guard':
          add({ who: 'tool', text: `Guard: ${m.text}`, ok: false })
          break
        case 'approval_request':
          setApproval(m.result as ApprovalRequest)
          break
        case 'approval_resolved':
          setApproval(null)
          add({ who: 'tool', text: m.ok ? 'Cloud request approved and sent' : 'Cloud request cancelled — nothing was sent', ok: m.ok })
          setAuditTick((n) => n + 1)
          break
      }
      if (m.type === 'tool_result' && m.name === 'ask_cloud') setAuditTick((n) => n + 1)
    }
    const connect = () => {
      const s = new WebSocket(`${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/ws`)
      s.binaryType = 'arraybuffer'
      s.onopen = () => {
        busy.current = false
        setStatus('ready')
        if (handsFreeRef.current) s.send(JSON.stringify({ type: 'listen_start' })) // resume after reconnect
        loadReminders()
        fetch('/api/status')
          .then((r) => r.json())
          .then((x) => setCloud(x.cloud))
          .catch(() => {})
      }
      s.onclose = () => {
        setStatus('offline')
        if (!closed) retry = window.setTimeout(connect, 1000)
      }
      s.onmessage = (e) => {
        if (!(e.data instanceof ArrayBuffer)) return onEvent(JSON.parse(e.data))
        if (Player.turnOf(e.data) === NOTIFY_TURN) {
          player.current?.play(e.data) // reminders always play
          return
        }
        if (Player.turnOf(e.data) !== turn.current || !player.current) return
        const wait = player.current.play(e.data)
        if (!heard.current) {
          heard.current = true
          setTiming((x) => ({ ...x, heard: Math.round(performance.now() - sentAt.current + wait) }))
        }
      }
      ws.current = s
    }
    connect()
    mic.current.onFrame = (pcm) => ws.current?.readyState === WebSocket.OPEN && ws.current.send(pcm)
    return () => {
      closed = true
      window.clearTimeout(retry)
      ws.current?.close()
    }
  }, [loadReminders])

  useEffect(() => {
    scroller.current?.scrollTo({ top: scroller.current.scrollHeight })
  }, [lines])

  const send = (m: object) => ws.current?.readyState === WebSocket.OPEN && ws.current.send(JSON.stringify(m))
  const connected = status !== 'connecting' && status !== 'offline'

  /** Stops the current turn's output locally; the server cancels it too. */
  const interrupt = () => {
    if (approvalRef.current) {
      getPlayer().stop() // answering an approval: keep the turn, silence the prompt
      return
    }
    turn.current = 0
    getPlayer().stop()
  }

  const decide = (approve: boolean) => {
    if (!approval) return
    send({ type: approve ? 'approve' : 'deny', id: approval.id })
  }

  const down = useCallback(async () => {
    if (!connected || status === 'listening') return
    interrupt()
    try {
      send({ type: 'ptt_start' })
      await mic.current.start()
      setStatus('listening')
    } catch (err) {
      add({ who: 'system', text: `Microphone unavailable: ${(err as Error).message}` })
    }
  }, [connected, status])

  const up = useCallback(() => {
    if (status !== 'listening') return
    if (!handsFreeRef.current) mic.current.stop() // hands-free keeps the mic streaming
    send({ type: 'ptt_end' })
    sentAt.current = performance.now()
    busy.current = true
    setStatus('thinking')
  }, [status])

  const toggleHandsFree = async () => {
    if (handsFree) {
      send({ type: 'listen_stop' })
      setHandsFree(false)
      setHearing(false)
      setAsleep(null)
      if (status !== 'listening') mic.current.stop()
      return
    }
    getPlayer() // created on this click so replies can play
    try {
      await mic.current.start()
      send({ type: 'listen_start' })
      setHandsFree(true)
    } catch (err) {
      add({ who: 'system', text: `Microphone unavailable: ${(err as Error).message}` })
    }
  }

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    const text = draft.trim()
    if (!text || !connected) return
    interrupt()
    send({ type: 'text', text })
    sentAt.current = performance.now()
    busy.current = true
    add({ who: 'you', text, meta: 'typed' })
    setDraft('')
    setStatus('thinking')
  }

  // Space bar = push to talk (when not typing).
  useEffect(() => {
    const kd = (e: KeyboardEvent) => {
      if (e.code === 'Space' && !e.repeat && spaceIsPushToTalk(e.target)) {
        e.preventDefault()
        void down()
      }
    }
    const ku = (e: KeyboardEvent) => {
      if (e.code === 'Space' && spaceIsPushToTalk(e.target)) {
        e.preventDefault()
        up()
      }
    }
    window.addEventListener('keydown', kd)
    window.addEventListener('keyup', ku)
    return () => {
      window.removeEventListener('keydown', kd)
      window.removeEventListener('keyup', ku)
    }
  }, [down, up])

  const label: Record<Status, string> = {
    connecting: 'Connecting…',
    ready: handsFree
      ? hearing
        ? 'Hearing you…'
        : asleep
          ? `Asleep — say “Hey ${asleep}” (or hold to talk)`
          : 'Listening — just talk (or hold to push-to-talk)'
      : 'Hold to talk',
    listening: 'Listening — release to send',
    thinking: 'Thinking… (hold to interrupt)',
    speaking: 'Speaking… (hold to interrupt)',
    offline: 'Server offline — retrying',
  }

  return (
    <main>
      <header>
        <h1>voc</h1>
        <span className={`dot ${hearing ? 'listening' : status}`} aria-hidden />
        <span className="muted">{status}</span>
        {cloud && (
          <span className={`cloud ${cloud.enabled ? 'on' : 'off'}`} title={cloud.enabled ? `${cloud.model} via ${cloud.destination}` : cloud.why}>
            {cloud.enabled ? 'cloud: ask first' : 'local only'}
          </span>
        )}
      </header>
      {approval && <ApprovalDialog req={approval} onDecide={decide} />}

      <section className="transcript" aria-live="polite" ref={scroller}>
        {lines.length === 0 && (
          <p className="muted">Hold the button (or the space bar) and speak, turn on hands-free to just talk, or type below.</p>
        )}
        {lines.map((l) => (
          <p key={l.key} className={`${l.who}${l.ok === false ? ' bad' : ''}`}>
            {l.who !== 'tool' && <b>{{ you: 'You', assistant: 'voc', system: 'System', reminder: '⏰' }[l.who]}</b>}{' '}
            {l.text}
            {l.partial && <span className="cursor">▍</span>}
            {l.meta && <span className="tag">{l.meta}</span>}
          </p>
        ))}
      </section>

      <button
        className={`ptt ${status}${handsFree && status === 'ready' ? (asleep ? ' asleep' : ' handsfree') : ''}${hearing ? ' hearing' : ''}`}
        data-ptt
        disabled={!connected}
        onPointerDown={(e) => {
          e.currentTarget.setPointerCapture(e.pointerId)
          void down()
        }}
        onPointerUp={up}
        onPointerCancel={up}
      >
        {label[status]}
      </button>

      <form className="compose" onSubmit={submit}>
        <button
          type="button"
          className={`hf${handsFree ? ' on' : ''}`}
          aria-pressed={handsFree}
          onClick={() => void toggleHandsFree()}
          disabled={!connected}
          title="Listen all the time; say “stop listening” to turn it off"
        >
          {handsFree ? 'Hands-free: on' : 'Hands-free: off'}
        </button>
        <input value={draft} onChange={(e) => setDraft(e.target.value)} placeholder="Or type a message…" aria-label="Message" />
        <button type="submit" disabled={!connected || !draft.trim()}>
          Send
        </button>
      </form>

      {reminders.length > 0 && (
        <section className="reminders" aria-label="Reminders">
          <h2>Reminders</h2>
          <ul>
            {reminders.map((r) => (
              <li key={r.id} className={r.status}>
                <span>{r.text}</span>
                <span className="muted">
                  {r.status === 'fired' ? 'done · ' : ''}
                  {fmtDue(r.due_at)}
                </span>
              </li>
            ))}
          </ul>
        </section>
      )}

      <AuditLog refresh={auditTick} />

      <footer className="timing" title="Milliseconds after you finished speaking">
        {STAGES.filter((k) => k in timing).map((k) => (
          <span key={k} className={k === 'heard' ? 'key' : undefined}>
            {k} <b>{timing[k]}</b>
          </span>
        ))}
      </footer>
    </main>
  )
}
