// Goal mode UI: the plan approval dialog and the live task panel.

export type GoalStep = { tool: string; intent: string; status?: string; note?: string; calls?: { ok: boolean }[] }
export type GoalPlan = { can_do: boolean; reason?: string; summary: string; steps: GoalStep[] }

const icon: Record<string, string> = { done: '✓', running: '…', failed: '✗', skipped: '–', interrupted: '⏸', pending: '·' }

export function PlanDialog({ goal, plan, onDecide }: { goal: string; plan: GoalPlan; onDecide: (ok: boolean) => void }) {
  return (
    <div className="overlay" role="dialog" aria-modal="true" aria-labelledby="plan-title">
      <div className="dialog">
        <h2 id="plan-title">Go ahead with this plan?</h2>
        <p className="muted small">{goal}</p>
        <p>{plan.summary}</p>
        <ol className="steps">
          {plan.steps.map((s, i) => (
            <li key={i} className={s.status}>
              <span>{s.intent}</span> <code>{s.tool}</code>
              {s.status === 'done' && <span className="muted small"> (already done)</span>}
            </li>
          ))}
        </ol>
        <div className="actions">
          <button className="deny" onClick={() => onDecide(false)}>
            Cancel
          </button>
          <button className="approve" onClick={() => onDecide(true)} autoFocus>
            Go ahead
          </button>
        </div>
        <p className="muted small">Or say “go ahead” or “cancel”.</p>
      </div>
    </div>
  )
}

export function TaskPanel({ goal, status, plan }: { goal: string; status: string; plan: GoalPlan }) {
  return (
    <section className="task" aria-label="Task" aria-live="polite">
      <h2>
        Task <span className={`badge ${status}`}>{status}</span>
      </h2>
      <p className="small">{goal}</p>
      <ol className="steps">
        {plan.steps.map((s, i) => (
          <li key={i} className={s.status}>
            <span className="icon" aria-hidden>
              {icon[s.status ?? 'pending'] ?? '·'}
            </span>{' '}
            {s.intent}
            {s.calls && s.calls.length > 1 && <span className="muted small"> ×{s.calls.length}</span>}
            {s.note && <span className="muted small"> — {s.note}</span>}
          </li>
        ))}
      </ol>
    </section>
  )
}
