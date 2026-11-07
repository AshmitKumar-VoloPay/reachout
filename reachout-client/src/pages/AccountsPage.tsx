import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { api } from '../api/endpoints'
import type { Account } from '../api/types'
import { Empty, Notice, StatusBadge } from '../components/ui'
import { timing } from '../config'
import { browserTimezone, errorMessage, formatDate, timezones } from '../lib/format'
import { ScheduleForm } from './ScheduleForm'

/**
 * Asks the server to re-check one account on an interval while `enabled`, until it is no
 * longer pending. Used while a LinkedIn login is being completed in another tab.
 */
function useAccountPolling(id: number | undefined, enabled: boolean, startedAt?: number, stopWhen: Account['status'][] = ['pending']) {
  const qc = useQueryClient()
  const q = useQuery({
    queryKey: ['account-poll', id],
    queryFn: () => api.accounts.refresh(id as number),
    enabled: enabled && id !== undefined,
    retry: false,
    refetchInterval: (query) => {
      const s = query.state.data?.status
      const timedOut = startedAt !== undefined && Date.now() - startedAt > timing.connectTimeoutMs
      if (timedOut) return false
      return !s || stopWhen.includes(s) ? timing.connectPollMs : false
    },
  })
  const status = q.data?.status
  const finished = status !== undefined && !stopWhen.includes(status)
  useEffect(() => {
    if (finished) void qc.invalidateQueries({ queryKey: ['accounts'] })
  }, [finished, qc])
  return q
}

export function AccountsPage() {
  const accounts = useQuery({ queryKey: ['accounts'], queryFn: api.accounts.list })
  const [connecting, setConnecting] = useState(false)

  return (
    <main>
      <header className="page-header">
        <div>
          <h1>LinkedIn accounts</h1>
          <p className="muted">Connect the LinkedIn account Reachout will send from, and choose when it may send.</p>
        </div>
        {!connecting && (
          <button type="button" className="primary" onClick={() => setConnecting(true)}>
            Connect LinkedIn account
          </button>
        )}
      </header>

      {connecting && <ConnectPanel onClose={() => setConnecting(false)} />}

      {accounts.isPending && <p aria-busy="true">Loading accounts…</p>}
      {accounts.isError && <Notice tone="error">{errorMessage(accounts.error)}</Notice>}
      {accounts.data && accounts.data.length === 0 && !connecting && (
        <Empty title="No LinkedIn account connected yet">
          Connect one to import leads from LinkedIn searches and run outreach.
        </Empty>
      )}
      <div className="stack">
        {accounts.data?.map((a) => (
          <AccountCard key={a.id} account={a} />
        ))}
      </div>
    </main>
  )
}

function ConnectPanel({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient()
  const [timezone, setTimezone] = useState(browserTimezone())
  const [attempt, setAttempt] = useState<{ id: number; link: string; startedAt: number } | null>(null)

  const start = useMutation({
    mutationFn: () => api.accounts.connect(timezone),
    onSuccess(acc) {
      const link = acc.connection_link ?? ''
      setAttempt({ id: acc.id, link, startedAt: Date.now() })
      void qc.invalidateQueries({ queryKey: ['accounts'] })
      // Browsers may block a window opened after a network call, so the link is also shown.
      if (link) window.open(link, '_blank', 'noopener')
    },
  })

  const poll = useAccountPolling(attempt?.id, attempt !== null, attempt?.startedAt)
  const status = poll.data?.status

  // Give up on an attempt that has been open too long (the polling above stops at the same time).
  const [expiredId, setExpiredId] = useState<number | null>(null)
  const attemptId = attempt?.id
  const attemptStart = attempt?.startedAt
  useEffect(() => {
    if (attemptId === undefined || attemptStart === undefined) return
    const t = setTimeout(() => setExpiredId(attemptId), Math.max(0, attemptStart + timing.connectTimeoutMs - Date.now()))
    return () => clearTimeout(t)
  }, [attemptId, attemptStart])
  const timedOut = attempt !== null && expiredId === attempt.id && (status === undefined || status === 'pending')

  if (!attempt) {
    return (
      <section className="panel" aria-label="Connect LinkedIn">
        <h2>Connect a LinkedIn account</h2>
        <p className="muted">
          You will sign in to LinkedIn on a page hosted by Linked API, our LinkedIn provider. Reachout never sees your
          LinkedIn password.
        </p>
        <div className="field">
          <label htmlFor="connect-tz">Account timezone</label>
          <select id="connect-tz" value={timezone} onChange={(e) => setTimezone(e.target.value)}>
            {timezones(timezone).map((z) => (
              <option key={z} value={z}>
                {z}
              </option>
            ))}
          </select>
          <p className="hint">Sending hours are counted in this timezone. You can change them later.</p>
        </div>
        {start.isError && <Notice tone="error">{errorMessage(start.error)}</Notice>}
        <div className="actions">
          <button type="button" className="primary" onClick={() => start.mutate()} disabled={start.isPending}>
            {start.isPending ? 'Starting…' : 'Continue to LinkedIn'}
          </button>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
        </div>
      </section>
    )
  }

  return (
    <section className="panel" aria-label="Connecting LinkedIn" aria-live="polite">
      <h2>Connecting your LinkedIn account</h2>
      {(status === undefined || status === 'pending') && !timedOut && (
        <>
          <ol className="steps">
            <li>
              A LinkedIn login page should have opened in a new tab.{' '}
              {attempt.link && (
                <a href={attempt.link} target="_blank" rel="noopener noreferrer">
                  Open it again
                </a>
              )}
            </li>
            <li>Sign in to LinkedIn there and finish.</li>
            <li>Come back to this page. It checks automatically.</li>
          </ol>
          <p className="muted">
            <span className="spinner" aria-hidden="true" /> Waiting for you to finish signing in…
          </p>
        </>
      )}
      {timedOut && <Notice tone="warn">This attempt took too long and has expired. Start again.</Notice>}
      {poll.isError && <Notice tone="error">{errorMessage(poll.error)}</Notice>}
      {status === 'connected' && <Notice tone="success">Connected as {poll.data?.name || 'your LinkedIn account'}.</Notice>}
      {status && status !== 'pending' && status !== 'connected' && (
        <Notice tone="error">
          The connection did not complete{poll.data?.status_detail ? `: ${poll.data.status_detail}` : '.'}
        </Notice>
      )}
      <div className="actions">
        {status === 'connected' ? (
          <button type="button" className="primary" onClick={onClose}>
            Done
          </button>
        ) : (
          <>
            {(timedOut || (status && status !== 'pending') || poll.isError) && (
              <button type="button" className="primary" onClick={() => setAttempt(null)}>
                Start again
              </button>
            )}
            <button type="button" onClick={onClose}>
              Close
            </button>
          </>
        )}
      </div>
    </section>
  )
}

function AccountCard({ account }: { account: Account }) {
  const qc = useQueryClient()
  const [showSchedule, setShowSchedule] = useState(false)
  const [reconnectLink, setReconnectLink] = useState('')
  const [reconnectStartedAt, setReconnectStartedAt] = useState<number>()

  const refresh = useMutation({
    mutationFn: () => api.accounts.refresh(account.id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['accounts'] }),
  })
  const reconnect = useMutation({
    mutationFn: () => api.accounts.reconnect(account.id),
    onSuccess(link) {
      setReconnectLink(link)
      setReconnectStartedAt(Date.now())
      window.open(link, '_blank', 'noopener')
    },
  })

  // A connection left over from an earlier visit, or a reconnection just started: keep checking.
  const watching = account.status === 'pending' || (account.status === 'reconnection_required' && reconnectLink !== '')
  useAccountPolling(account.id, watching, reconnectStartedAt, ['pending', 'reconnection_required'])

  const usable = account.status !== 'pending' && account.status !== 'error'

  return (
    <article className="card" aria-label={account.name || 'LinkedIn account'}>
      <div className="card-head">
        <div className="avatar" aria-hidden="true">
          {(account.name || '?').slice(0, 1).toUpperCase()}
        </div>
        <div className="card-title">
          <h2>{account.name || 'LinkedIn account'}</h2>
          {account.headline && <p className="muted">{account.headline}</p>}
          {account.profile_url && (
            <a href={account.profile_url} target="_blank" rel="noopener noreferrer">
              View profile
            </a>
          )}
        </div>
        <div className="card-status">
          <StatusBadge status={account.status} />
          {account.connected_at && <span className="muted small">since {formatDate(account.connected_at)}</span>}
        </div>
      </div>

      {account.status === 'pending' && (
        <Notice tone="info">
          A connection is in progress. If you closed the LinkedIn login tab, it expires after about 10 minutes and you can
          start again.
        </Notice>
      )}
      {account.status === 'reconnection_required' && (
        <Notice tone="warn">
          LinkedIn signed this account out, so nothing is being sent. Reconnect to continue.
          {reconnectLink && (
            <>
              {' '}
              <a href={reconnectLink} target="_blank" rel="noopener noreferrer">
                Open the LinkedIn login page
              </a>
            </>
          )}
        </Notice>
      )}
      {account.status === 'frozen' && (
        <Notice tone="warn">This account is frozen, usually because the Linked API subscription lapsed. Nothing is being sent.</Notice>
      )}
      {account.status === 'disconnected' && <Notice tone="error">This account was removed in Linked API. Connect it again to continue.</Notice>}
      {account.status === 'error' && (
        <Notice tone="error">The connection did not complete{account.status_detail ? `: ${account.status_detail}` : '.'} Connect it again.</Notice>
      )}
      {(refresh.isError || reconnect.isError) && <Notice tone="error">{errorMessage(refresh.error ?? reconnect.error)}</Notice>}

      <div className="actions">
        {account.status === 'reconnection_required' && (
          <button type="button" className="primary" onClick={() => reconnect.mutate()} disabled={reconnect.isPending}>
            {reconnect.isPending ? 'Starting…' : 'Reconnect'}
          </button>
        )}
        <button type="button" onClick={() => refresh.mutate()} disabled={refresh.isPending}>
          {refresh.isPending ? 'Checking…' : 'Check status'}
        </button>
        {usable && (
          <button type="button" aria-expanded={showSchedule} onClick={() => setShowSchedule((s) => !s)}>
            {showSchedule ? 'Hide sending schedule' : 'Sending schedule'}
          </button>
        )}
      </div>

      {usable && showSchedule && (
        <div className="card-section">
          <h3>Sending schedule</h3>
          <ScheduleForm accountId={account.id} />
        </div>
      )}
    </article>
  )
}
