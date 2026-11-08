import { useEffect, useState } from 'react'

type AuditEntry = {
  id: number
  created_at: string
  decision: string
  destination: string
  model: string
  reason: string
  detail?: string
  sent?: string
  sent_sha256?: string
  response?: string
}

/** Outbound log: every escalation decision and the exact bytes sent. */
export function AuditLog({ refresh }: { refresh: number }) {
  const [entries, setEntries] = useState<AuditEntry[]>([])
  useEffect(() => {
    fetch('/api/audit')
      .then((r) => (r.ok ? r.json() : []))
      .then(setEntries)
      .catch(() => {})
  }, [refresh])
  if (entries.length === 0) return null
  return (
    <details className="audit">
      <summary>
        Outbound log <span className="muted">({entries.length})</span>
      </summary>
      <ul>
        {entries.map((e) => (
          <li key={e.id}>
            <div className="row">
              <span className={`badge ${e.decision}`}>{e.decision}</span>
              <span>{e.reason || 'cloud request'}</span>
              <span className="muted">{new Date(e.created_at).toLocaleTimeString()}</span>
            </div>
            <div className="muted small">
              {e.destination} · {e.model}
              {e.detail ? ` · ${e.detail}` : ''}
            </div>
            {e.sent ? (
              <details>
                <summary className="small">
                  Sent {e.sent.length.toLocaleString()} bytes · sha256 {e.sent_sha256?.slice(0, 12)}…
                </summary>
                <pre className="payload">{pretty(e.sent)}</pre>
                {e.response && <p className="small">Response: {e.response}</p>}
              </details>
            ) : (
              <div className="muted small">Nothing left this device.</div>
            )}
          </li>
        ))}
      </ul>
    </details>
  )
}

function pretty(s: string) {
  try {
    return JSON.stringify(JSON.parse(s), null, 2)
  } catch {
    return s
  }
}
