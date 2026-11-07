import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/endpoints'
import type { Lead } from '../api/types'
import { Empty, Notice } from '../components/ui'
import { PAGE_SIZE, timing } from '../config'
import { errorMessage, formatDate } from '../lib/format'
import { useDebounced } from '../lib/hooks'
import { ImportPanel } from './ImportPanel'

const EMPTY: Set<number> = new Set()

export function LeadsPage() {
  const qc = useQueryClient()
  const [search, setSearch] = useState('')
  const q = useDebounced(search.trim(), timing.debounceMs)
  const [importing, setImporting] = useState(false)
  // The page and the selection belong to one search text: a new search starts at the first
  // page with nothing selected, without resetting state from an effect.
  const [pos, setPos] = useState({ q: '', offset: 0 })
  const offset = pos.q === q ? pos.offset : 0
  const setOffset = (o: number) => setPos({ q, offset: o })
  const [sel, setSel] = useState<{ key: string; ids: Set<number> }>({ key: '', ids: new Set() })
  const selKey = `${q}|${offset}`
  const selected = sel.key === selKey ? sel.ids : EMPTY
  const setSelected = (ids: Set<number>) => setSel({ key: selKey, ids })
  const [confirming, setConfirming] = useState<'none' | 'selected' | number>('none')
  const [message, setMessage] = useState<{ tone: 'success' | 'warn' | 'error'; text: string } | null>(null)

  const leads = useQuery({
    queryKey: ['leads', q, offset],
    queryFn: () => api.leads.list(q, PAGE_SIZE, offset),
    placeholderData: keepPreviousData,
  })
  const page = leads.data

  function afterDelete() {
    setSelected(new Set())
    setConfirming('none')
    void qc.invalidateQueries({ queryKey: ['leads'] })
  }

  const removeOne = useMutation({
    mutationFn: (id: number) => api.leads.remove(id),
    onSuccess() {
      setMessage({ tone: 'success', text: 'Lead deleted.' })
      // Deleting the last lead on a later page: step back so the page is not empty.
      if (page && page.leads.length === 1 && offset > 0) setOffset(Math.max(0, offset - PAGE_SIZE))
      afterDelete()
    },
    onError(err) {
      setConfirming('none')
      setMessage({ tone: 'error', text: errorMessage(err) })
    },
  })

  const removeMany = useMutation({
    mutationFn: (ids: number[]) => api.leads.bulkRemove(ids),
    onSuccess(r, ids) {
      const parts = [`Deleted ${r.deleted} ${r.deleted === 1 ? 'lead' : 'leads'}.`]
      if (r.blocked > 0) {
        parts.push(
          `${r.blocked} ${r.blocked === 1 ? 'lead is' : 'leads are'} in a running or paused campaign and ${
            r.blocked === 1 ? 'was' : 'were'
          } kept.`,
        )
      }
      setMessage({ tone: r.blocked > 0 ? 'warn' : 'success', text: parts.join(' ') })
      if (page && r.deleted >= page.leads.length && offset > 0 && ids.length >= page.leads.length) {
        setOffset(Math.max(0, offset - PAGE_SIZE))
      }
      afterDelete()
    },
    onError(err) {
      setConfirming('none')
      setMessage({ tone: 'error', text: errorMessage(err) })
    },
  })

  const rows = page?.leads ?? []
  const allSelected = rows.length > 0 && rows.every((l) => selected.has(l.id))

  function toggle(id: number) {
    const n = new Set(selected)
    if (n.has(id)) n.delete(id)
    else n.add(id)
    setSelected(n)
  }

  const from = page && page.total > 0 ? page.offset + 1 : 0
  const to = page ? page.offset + rows.length : 0

  return (
    <main>
      <header className="page-header">
        <div>
          <h1>Leads</h1>
          <p className="muted">
            {page ? `${page.total} ${page.total === 1 ? 'lead' : 'leads'}` : 'People you plan to contact.'}
          </p>
        </div>
        {!importing && (
          <button type="button" className="primary" onClick={() => setImporting(true)}>
            Import leads
          </button>
        )}
      </header>

      {importing && <ImportPanel onClose={() => setImporting(false)} />}
      {message && <Notice tone={message.tone}>{message.text}</Notice>}

      <div className="toolbar">
        <div className="field grow">
          <label htmlFor="lead-search" className="sr-only">
            Search leads
          </label>
          <input
            id="lead-search"
            type="search"
            placeholder="Search by name, headline, company or URL"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
        </div>
        {selected.size > 0 &&
          (confirming === 'selected' ? (
            <span className="confirm">
              Delete {selected.size} {selected.size === 1 ? 'lead' : 'leads'}?{' '}
              <button type="button" className="danger" onClick={() => removeMany.mutate([...selected])} disabled={removeMany.isPending}>
                Yes, delete
              </button>
              <button type="button" onClick={() => setConfirming('none')}>
                Cancel
              </button>
            </span>
          ) : (
            <button type="button" className="danger" onClick={() => setConfirming('selected')}>
              Delete selected ({selected.size})
            </button>
          ))}
      </div>

      {leads.isPending && <p aria-busy="true">Loading leads…</p>}
      {leads.isError && <Notice tone="error">{errorMessage(leads.error)}</Notice>}

      {page && page.total === 0 && q === '' && (
        <Empty title="No leads yet">
          Import profile URLs or run a LinkedIn search to add people. To search, first{' '}
          <Link to="/accounts">connect a LinkedIn account</Link>.
        </Empty>
      )}
      {page && page.total === 0 && q !== '' && <Empty title="No leads match your search">Try a different name or company.</Empty>}

      {rows.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th className="check-col">
                  <input
                    type="checkbox"
                    aria-label="Select all leads on this page"
                    checked={allSelected}
                    onChange={() => setSelected(allSelected ? new Set() : new Set(rows.map((l) => l.id)))}
                  />
                </th>
                <th>Name</th>
                <th>Headline</th>
                <th>Company</th>
                <th>Added</th>
                <th>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {rows.map((l) => (
                <LeadRow
                  key={l.id}
                  lead={l}
                  selected={selected.has(l.id)}
                  onToggle={() => toggle(l.id)}
                  confirming={confirming === l.id}
                  onAsk={() => setConfirming(l.id)}
                  onCancel={() => setConfirming('none')}
                  onConfirm={() => removeOne.mutate(l.id)}
                  busy={removeOne.isPending}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {page && page.total > 0 && (
        <nav className="pager" aria-label="Pages">
          <span className="muted">
            Showing {from}–{to} of {page.total}
          </span>
          <span className="actions">
            <button type="button" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}>
              Previous
            </button>
            <button type="button" disabled={to >= page.total} onClick={() => setOffset(offset + PAGE_SIZE)}>
              Next
            </button>
          </span>
        </nav>
      )}
    </main>
  )
}

function LeadRow(props: {
  lead: Lead
  selected: boolean
  onToggle: () => void
  confirming: boolean
  onAsk: () => void
  onCancel: () => void
  onConfirm: () => void
  busy: boolean
}) {
  const { lead: l } = props
  const label = l.name || l.profile_url
  return (
    <tr>
      <td className="check-col">
        <input type="checkbox" aria-label={`Select ${label}`} checked={props.selected} onChange={props.onToggle} />
      </td>
      <td>
        <a href={l.profile_url} target="_blank" rel="noopener noreferrer">
          {l.name || <span className="muted">{l.profile_url.replace('https://www.linkedin.com', '')}</span>}
        </a>
      </td>
      <td>{l.headline}</td>
      <td>{l.company}</td>
      <td className="nowrap">{formatDate(l.created_at)}</td>
      <td className="row-actions">
        {props.confirming ? (
          <>
            <button type="button" className="danger" onClick={props.onConfirm} disabled={props.busy} aria-label={`Confirm delete ${label}`}>
              Confirm
            </button>
            <button type="button" onClick={props.onCancel}>
              Cancel
            </button>
          </>
        ) : (
          <button type="button" onClick={props.onAsk} aria-label={`Delete ${label}`}>
            Delete
          </button>
        )}
      </td>
    </tr>
  )
}
