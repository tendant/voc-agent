// Approval dialog for cloud escalation: shows exactly what would leave the
// device, what was withheld, and what was redacted.

export type PolicyItem = { title: string; path: string; privacy: string; why?: string; redactions?: Record<string, number> }
export type ApprovalRequest = {
  id: string
  destination: string
  model: string
  reason: string
  payload: unknown
  policy: { included: PolicyItem[] | null; excluded: PolicyItem[] | null; note: string; warnings?: string[] }
}

function redactionText(r?: Record<string, number>) {
  if (!r) return ''
  return Object.entries(r)
    .map(([k, n]) => `${n} ${k}${n > 1 ? 's' : ''}`)
    .join(', ')
}

export function ApprovalDialog({ req, onDecide }: { req: ApprovalRequest; onDecide: (approve: boolean) => void }) {
  const included = req.policy.included ?? []
  const excluded = req.policy.excluded ?? []
  return (
    <div className="overlay" role="dialog" aria-modal="true" aria-labelledby="approval-title">
      <div className="dialog">
        <h2 id="approval-title">Send this to the cloud?</h2>
        <dl className="facts">
          <dt>Destination</dt>
          <dd>{req.destination}</dd>
          <dt>Model</dt>
          <dd>{req.model}</dd>
          <dt>Why</dt>
          <dd>{req.reason || '—'}</dd>
        </dl>

        {req.policy.warnings && req.policy.warnings.length > 0 && (
          <div className="warn" role="alert">
            <b>Check before sending:</b> the question mentions words that otherwise appear only in your local-only notes:{' '}
            {req.policy.warnings.map((w) => (
              <code key={w}>{w}</code>
            ))}
          </div>
        )}
        {excluded.length > 0 && (
          <div className="kept">
            <b>Stays on this device</b>
            <ul>
              {excluded.map((e) => (
                <li key={e.path}>
                  {e.title} <span className="muted">— {e.why}</span>
                </li>
              ))}
            </ul>
          </div>
        )}
        {included.length > 0 && (
          <div className="sent">
            <b>Included</b>
            <ul>
              {included.map((e) => (
                <li key={e.path || e.title}>
                  {e.title} <span className="muted">({e.privacy}{e.redactions ? `, redacted ${redactionText(e.redactions)}` : ''})</span>
                </li>
              ))}
            </ul>
          </div>
        )}
        <p className="muted small">{req.policy.note}</p>

        <details open>
          <summary>Exact request body</summary>
          <pre className="payload">{JSON.stringify(req.payload, null, 2)}</pre>
        </details>

        <div className="actions">
          <button className="deny" onClick={() => onDecide(false)}>
            Cancel
          </button>
          <button className="approve" onClick={() => onDecide(true)} autoFocus>
            Approve and send
          </button>
        </div>
        <p className="muted small">Or hold to talk and say “approve” or “cancel”.</p>
      </div>
    </div>
  )
}
