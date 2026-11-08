import { useCallback, useEffect, useRef, useState } from 'react'
import { Mic, Player } from './audio'

type Line = { id: number; who: 'you' | 'system'; text: string; meta?: string }
type Status = 'connecting' | 'ready' | 'listening' | 'thinking' | 'offline'

export default function App() {
  const ws = useRef<WebSocket | null>(null)
  const mic = useRef(new Mic())
  const player = useRef<Player | null>(null)
  const [status, setStatus] = useState<Status>('connecting')
  const [lines, setLines] = useState<Line[]>([])
  const [timing, setTiming] = useState<Record<string, number>>({})
  const [speaking, setSpeaking] = useState(false)
  const [echo, setEcho] = useState(true)
  const echoRef = useRef(echo)
  echoRef.current = echo
  const nextId = useRef(0)

  const add = (l: Omit<Line, 'id'>) => setLines((ls) => [...ls, { ...l, id: nextId.current++ }])

  useEffect(() => {
    let closed = false
    let retry: number | undefined
    const connect = () => {
      const s = new WebSocket(`${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/ws`)
      s.binaryType = 'arraybuffer'
      s.onopen = () => setStatus('ready')
      s.onclose = () => {
        setStatus('offline')
        if (!closed) retry = window.setTimeout(connect, 1000)
      }
      s.onmessage = (e) => {
        if (e.data instanceof ArrayBuffer) {
          if (echoRef.current) player.current?.play(e.data)
          return
        }
        const m = JSON.parse(e.data)
        if (m.type === 'transcript') {
          setStatus('ready')
          add({ who: 'you', text: m.text || '(no speech detected)', meta: m.lang })
        } else if (m.type === 'timing') {
          setTiming(m.ms)
        } else if (m.type === 'error') {
          setStatus('ready')
          add({ who: 'system', text: m.error })
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
  }, [])

  const send = (m: object) => ws.current?.readyState === WebSocket.OPEN && ws.current.send(JSON.stringify(m))

  const down = useCallback(async () => {
    if (status !== 'ready') return
    if (!player.current) {
      player.current = new Player() // created on a user gesture so autoplay is allowed
      player.current.onLevel = setSpeaking
    }
    try {
      send({ type: 'ptt_start' })
      await mic.current.start()
      setStatus('listening')
    } catch (err) {
      add({ who: 'system', text: `Microphone unavailable: ${(err as Error).message}` })
    }
  }, [status])

  const up = useCallback(() => {
    if (status !== 'listening') return
    mic.current.stop()
    send({ type: 'ptt_end' })
    setStatus('thinking')
  }, [status])

  // Space bar = push to talk.
  useEffect(() => {
    const kd = (e: KeyboardEvent) => {
      if (e.code === 'Space' && !e.repeat && e.target === document.body) {
        e.preventDefault()
        void down()
      }
    }
    const ku = (e: KeyboardEvent) => {
      if (e.code === 'Space' && e.target === document.body) {
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
    ready: 'Hold to talk',
    listening: 'Listening — release to send',
    thinking: 'Transcribing…',
    offline: 'Server offline — retrying',
  }

  return (
    <main>
      <header>
        <h1>voc</h1>
        <span className={`dot ${status}`} aria-label={status} />
        <span className="muted">{status}</span>
      </header>

      <section className="transcript" aria-live="polite">
        {lines.length === 0 && <p className="muted">Hold the button (or the space bar) and speak.</p>}
        {lines.map((l) => (
          <p key={l.id} className={l.who}>
            <b>{l.who === 'you' ? 'You' : 'System'}</b> {l.text}
            {l.meta && <span className="tag">{l.meta}</span>}
          </p>
        ))}
      </section>

      <button
        className={`ptt ${status}`}
        disabled={status === 'connecting' || status === 'offline' || status === 'thinking'}
        onPointerDown={(e) => {
          e.currentTarget.setPointerCapture(e.pointerId)
          void down()
        }}
        onPointerUp={up}
        onPointerCancel={up}
      >
        {label[status]}
      </button>

      <footer>
        <label>
          <input type="checkbox" checked={echo} onChange={(e) => setEcho(e.target.checked)} /> Play echo
        </label>
        {speaking && <span className="muted">playing…</span>}
        <span className="timing">
          {Object.entries(timing).map(([k, v]) => (
            <span key={k}>
              {k} {v} ms
            </span>
          ))}
        </span>
      </footer>
    </main>
  )
}
