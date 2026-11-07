import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState, type FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/endpoints'
import type { PasteImportResult, SearchImport } from '../api/types'
import { Notice } from '../components/ui'
import { timing } from '../config'
import { errorMessage } from '../lib/format'

type Tab = 'paste' | 'search'

export function ImportPanel({ onClose }: { onClose: () => void }) {
  const [tab, setTab] = useState<Tab>('paste')
  return (
    <section className="panel" aria-label="Import leads">
      <div className="panel-head">
        <h2>Import leads</h2>
        <button type="button" onClick={onClose}>
          Close
        </button>
      </div>
      <div role="tablist" aria-label="Import method" className="tabs">
        <button type="button" role="tab" aria-selected={tab === 'paste'} onClick={() => setTab('paste')}>
          Paste profile URLs
        </button>
        <button type="button" role="tab" aria-selected={tab === 'search'} onClick={() => setTab('search')}>
          From a LinkedIn search
        </button>
      </div>
      <div role="tabpanel">{tab === 'paste' ? <PasteImport /> : <SearchImportForm />}</div>
    </section>
  )
}

function plural(n: number, one: string, many: string) {
  return `${n} ${n === 1 ? one : many}`
}

function PasteImport() {
  const qc = useQueryClient()
  const [text, setText] = useState('')
  const [result, setResult] = useState<PasteImportResult | null>(null)

  const run = useMutation({
    mutationFn: () => api.leads.importUrls(text),
    onSuccess(r) {
      setResult(r)
      setText('')
      void qc.invalidateQueries({ queryKey: ['leads'] })
    },
  })

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    setResult(null)
    run.mutate()
  }

  return (
    <form onSubmit={onSubmit}>
      <div className="field">
        <label htmlFor="paste-urls">LinkedIn profile URLs</label>
        <textarea
          id="paste-urls"
          rows={6}
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder={'https://www.linkedin.com/in/jane-doe\nhttps://www.linkedin.com/in/john-roe'}
          aria-describedby="paste-hint"
        />
        <p id="paste-hint" className="hint">
          One per line, or separated by commas. Up to 500 at a time. Different spellings of the same profile count once.
        </p>
      </div>
      {run.isError && <Notice tone="error">{errorMessage(run.error)}</Notice>}
      {result && <PasteSummary result={result} />}
      <button type="submit" className="primary" disabled={run.isPending || text.trim() === ''}>
        {run.isPending ? 'Importing…' : 'Import'}
      </button>
    </form>
  )
}

function PasteSummary({ result }: { result: PasteImportResult }) {
  const parts = [
    `${plural(result.imported, 'lead', 'leads')} added`,
    result.duplicates > 0 ? `${plural(result.duplicates, 'duplicate', 'duplicates')} skipped` : '',
    result.over_cap > 0 ? `${plural(result.over_cap, 'lead', 'leads')} skipped because the lead limit was reached` : '',
  ].filter(Boolean)
  return (
    <div className="result">
      <Notice tone={result.imported > 0 ? 'success' : 'info'}>{parts.join(', ')}.</Notice>
      {result.invalid.length > 0 && (
        <details>
          <summary>{plural(result.invalid.length, 'entry', 'entries')} could not be used</summary>
          <ul className="invalid">
            {result.invalid.map((i, n) => (
              <li key={`${n}-${i.input}`}>
                <code>{i.input}</code> <span className="muted">{i.reason}</span>
              </li>
            ))}
          </ul>
        </details>
      )}
    </div>
  )
}

function SearchImportForm() {
  const qc = useQueryClient()
  const accounts = useQuery({ queryKey: ['accounts'], queryFn: api.accounts.list })
  const connected = (accounts.data ?? []).filter((a) => a.status === 'connected')

  const [accountId, setAccountId] = useState<number | ''>('')
  const [url, setUrl] = useState('')
  const [limit, setLimit] = useState(25)
  const [jobId, setJobId] = useState<number | null>(null)

  const chosen = accountId === '' ? connected[0]?.id : accountId

  const start = useMutation({
    mutationFn: () => api.leads.startSearch(chosen as number, url.trim(), limit),
    onSuccess: (job) => setJobId(job.id),
  })

  const job = useQuery({
    queryKey: ['import', jobId],
    queryFn: () => api.leads.getImport(jobId as number),
    enabled: jobId !== null,
    retry: false,
    refetchInterval: (q) => (q.state.data?.status === 'running' || !q.state.data ? timing.importPollMs : false),
  })
  // New leads exist once the search completes: refresh the table.
  const completed = job.data?.status === 'completed'
  useEffect(() => {
    if (completed) void qc.invalidateQueries({ queryKey: ['leads'] })
  }, [completed, qc])

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    setJobId(null)
    start.mutate()
  }

  if (accounts.isPending) return <p aria-busy="true">Loading accounts…</p>
  if (connected.length === 0) {
    return (
      <Notice tone="info">
        Searching needs a connected LinkedIn account. <Link to="/accounts">Connect one</Link> first, or paste profile URLs
        instead.
      </Notice>
    )
  }

  const running = jobId !== null && (job.data === undefined || job.data.status === 'running') && !job.isError

  return (
    <form onSubmit={onSubmit}>
      {connected.length > 1 && (
        <div className="field">
          <label htmlFor="search-account">LinkedIn account</label>
          <select
            id="search-account"
            value={chosen}
            onChange={(e) => setAccountId(Number(e.target.value))}
          >
            {connected.map((a) => (
              <option key={a.id} value={a.id}>
                {a.name || `Account ${a.id}`}
              </option>
            ))}
          </select>
        </div>
      )}
      <div className="field">
        <label htmlFor="search-url">LinkedIn people search URL</label>
        <input
          id="search-url"
          type="url"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
          placeholder="https://www.linkedin.com/search/results/people/?keywords=cto"
          aria-describedby="search-hint"
        />
        <p id="search-hint" className="hint">
          Run a people search on LinkedIn with your filters, then copy the address from the browser. Searches only run
          during the account&apos;s working hours and can take a few minutes.
        </p>
      </div>
      <div className="field narrow">
        <label htmlFor="search-limit">How many people</label>
        <input
          id="search-limit"
          type="number"
          min={1}
          max={100}
          value={Number.isNaN(limit) ? '' : limit}
          onChange={(e) => setLimit(e.target.valueAsNumber)}
        />
        <p className="hint">1 to 100.</p>
      </div>

      {start.isError && <Notice tone="error">{errorMessage(start.error)}</Notice>}
      {job.isError && <Notice tone="error">{errorMessage(job.error)}</Notice>}
      {jobId !== null && job.data && <SearchJobStatus job={job.data} />}

      <button
        type="submit"
        className="primary"
        disabled={start.isPending || running || url.trim() === '' || !(limit >= 1 && limit <= 100)}
      >
        {start.isPending ? 'Starting…' : running ? 'Searching…' : 'Import from search'}
      </button>
    </form>
  )
}

function SearchJobStatus({ job }: { job: SearchImport }) {
  if (job.status === 'running') {
    return (
      <p className="muted" role="status">
        <span className="spinner" aria-hidden="true" />{' '}
        {job.pending_reason === 'outsideWorkingHours'
          ? "Linked API is holding this search until the account's working hours there open."
          : job.pending_reason === 'queued'
            ? 'Waiting for LinkedIn. Other tasks for this account are ahead of it.'
            : 'Searching LinkedIn…'}
      </p>
    )
  }
  if (job.status === 'failed') return <Notice tone="error">The search failed: {job.message || 'unknown error'}.</Notice>
  const parts = [
    `${plural(job.found, 'person', 'people')} found`,
    `${plural(job.imported, 'new lead', 'new leads')} added`,
    job.duplicates > 0 ? `${plural(job.duplicates, 'duplicate', 'duplicates')} skipped` : '',
    job.invalid > 0 ? `${plural(job.invalid, 'result', 'results')} without a usable profile link` : '',
    job.over_cap > 0 ? `${plural(job.over_cap, 'lead', 'leads')} skipped because the lead limit was reached` : '',
  ].filter(Boolean)
  return <Notice tone={job.imported > 0 ? 'success' : 'info'}>{parts.join(', ')}.</Notice>
}
