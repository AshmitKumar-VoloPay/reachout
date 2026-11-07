import { fireEvent, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { SearchImport } from '../api/types'
import { account, apiError, lead, leadsPage, mockApi, sequence, signedIn } from '../test/mockApi'
import { renderApp } from '../test/render'

function job(over: Partial<SearchImport> = {}): SearchImport {
  return {
    id: 5, account_id: 1, search_url: 'https://www.linkedin.com/search/results/people/?keywords=cto', requested: 25,
    status: 'running', found: 0, imported: 0, duplicates: 0, invalid: 0, over_cap: 0, created_at: '2026-11-10T10:00:00Z', ...over,
  }
}

async function openImport(routes: Parameters<typeof mockApi>[0] = {}) {
  const api = mockApi({ ...signedIn(), ...routes })
  renderApp('/leads')
  const u = userEvent.setup()
  await u.click(await screen.findByRole('button', { name: 'Import leads' }))
  return { api, u }
}

describe('pasting profile URLs', () => {
  it('imports the pasted text and reports what happened', async () => {
    const { api, u } = await openImport({
      'POST /api/leads/import/urls': {
        body: { imported: 3, duplicates: 1, over_cap: 0, invalid: [{ input: 'https://example.com/x', reason: 'not a linkedin.com URL' }] },
      },
    })
    expect(screen.getByRole('button', { name: 'Import' })).toBeDisabled() // nothing to import yet
    await u.type(screen.getByLabelText('LinkedIn profile URLs'), 'linkedin.com/in/jane{enter}https://example.com/x')
    await u.click(screen.getByRole('button', { name: 'Import' }))

    expect(await screen.findByText(/3 leads added, 1 duplicate skipped/i)).toBeInTheDocument()
    expect(api.called('POST', '/api/leads/import/urls')[0].body).toEqual({ text: 'linkedin.com/in/jane\nhttps://example.com/x' })
    // The entries that could not be used are listed, with the reason.
    await u.click(screen.getByText(/1 entry could not be used/i))
    expect(screen.getByText('https://example.com/x')).toBeInTheDocument()
    expect(screen.getByText('not a linkedin.com URL')).toBeInTheDocument()
    expect(screen.getByLabelText('LinkedIn profile URLs')).toHaveValue('') // cleared for the next paste
  })

  it('refreshes the table with the new leads', async () => {
    let added = false
    const { u } = await openImport({
      'GET /api/leads': () => ({ body: leadsPage(added ? [lead(1, { name: 'Jane Doe' })] : []) }),
      'POST /api/leads/import/urls': () => {
        added = true
        return { body: { imported: 1, duplicates: 0, over_cap: 0, invalid: [] } }
      },
    })
    await u.type(screen.getByLabelText('LinkedIn profile URLs'), 'linkedin.com/in/jane-doe')
    await u.click(screen.getByRole('button', { name: 'Import' }))
    expect(await screen.findByRole('link', { name: 'Jane Doe' })).toBeInTheDocument()
  })

  it('mentions leads skipped because of the limit, and a result with nothing new', async () => {
    const { u } = await openImport({
      'POST /api/leads/import/urls': { body: { imported: 0, duplicates: 2, over_cap: 4, invalid: [] } },
    })
    await u.type(screen.getByLabelText('LinkedIn profile URLs'), 'x')
    await u.click(screen.getByRole('button', { name: 'Import' }))
    const note = await screen.findByText(/0 leads added/i)
    expect(note).toHaveTextContent('2 duplicates skipped')
    expect(note).toHaveTextContent('4 leads skipped because the lead limit was reached')
    expect(screen.queryByText(/could not be used/i)).not.toBeInTheDocument()
  })

  it('shows the server\'s reason when the paste is refused', async () => {
    const { u } = await openImport({
      'POST /api/leads/import/urls': apiError(400, 'invalid_input', 'invalid input: at most 500 URLs per import'),
    })
    await u.type(screen.getByLabelText('LinkedIn profile URLs'), 'x')
    await u.click(screen.getByRole('button', { name: 'Import' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/at most 500 URLs/i)
    expect(screen.getByLabelText('LinkedIn profile URLs')).toHaveValue('x') // not lost
  })

  it('can be closed', async () => {
    const { u } = await openImport()
    await u.click(screen.getByRole('button', { name: 'Close' }))
    expect(screen.queryByLabelText('LinkedIn profile URLs')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Import leads' })).toBeInTheDocument()
  })
})

describe('importing from a LinkedIn search', () => {
  const connected = { 'GET /api/accounts': { body: { accounts: [account({ id: 1, name: 'Ada' })] } } }

  async function openSearch(routes: Parameters<typeof mockApi>[0] = {}) {
    const ctx = await openImport({ ...connected, ...routes })
    await ctx.u.click(screen.getByRole('tab', { name: 'From a LinkedIn search' }))
    return ctx
  }

  it('needs a connected account and points to the connect page', async () => {
    const { u } = await openImport() // no accounts
    await u.click(screen.getByRole('tab', { name: 'From a LinkedIn search' }))
    expect(await screen.findByText(/searching needs a connected linkedin account/i)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Connect one' })).toHaveAttribute('href', '/accounts')
  })

  it('ignores accounts that cannot search', async () => {
    const { u } = await openImport({
      'GET /api/accounts': { body: { accounts: [account({ status: 'reconnection_required' }), account({ id: 2, status: 'frozen' })] } },
    })
    await u.click(screen.getByRole('tab', { name: 'From a LinkedIn search' }))
    expect(await screen.findByText(/searching needs a connected linkedin account/i)).toBeInTheDocument()
  })

  it('starts the search, follows it while it runs, then shows the result and the new leads', async () => {
    let done = false
    const { api, u } = await openSearch({
      'GET /api/leads': () => ({ body: leadsPage(done ? [lead(1, { name: 'Found Person' })] : []) }),
      'POST /api/leads/import/search': { status: 202, body: { import: job({ pending_reason: 'queued' }) } },
      'GET /api/leads/imports/:id': sequence(
        { body: { import: job({ pending_reason: 'queued' }) } },
        { body: { import: job({ pending_reason: 'outsideWorkingHours' }) } },
        { body: { import: job({ status: 'completed', found: 5, imported: 3, duplicates: 1, invalid: 1 }) } },
      ),
    })
    await u.type(screen.getByLabelText('LinkedIn people search URL'), 'https://www.linkedin.com/search/results/people/?keywords=cto')
    fireEvent.change(screen.getByLabelText('How many people'), { target: { value: '10' } })
    await u.click(screen.getByRole('button', { name: 'Import from search' }))

    expect(api.called('POST', '/api/leads/import/search')[0].body).toEqual({
      account_id: 1, search_url: 'https://www.linkedin.com/search/results/people/?keywords=cto', limit: 10,
    })
    expect(await screen.findByText(/other tasks for this account are ahead of it/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Searching…' })).toBeDisabled() // one search at a time
    expect(await screen.findByText(/holding this search until the account's working hours/i)).toBeInTheDocument()

    done = true
    const summary = await screen.findByText(/5 people found, 3 new leads added/i)
    expect(summary).toHaveTextContent('1 duplicate skipped')
    expect(summary).toHaveTextContent('1 result without a usable profile link')
    expect(await screen.findByRole('link', { name: 'Found Person' })).toBeInTheDocument() // table refreshed
    expect(screen.getByRole('button', { name: 'Import from search' })).toBeEnabled()

    // Polling stops once it has finished.
    const n = api.called('GET', '/api/leads/imports/5').length
    await new Promise((r) => setTimeout(r, 80))
    expect(api.called('GET', '/api/leads/imports/5').length).toBe(n)
  })

  it('shows why a search failed', async () => {
    const { u } = await openSearch({
      'POST /api/leads/import/search': { status: 202, body: { import: job() } },
      'GET /api/leads/imports/:id': { body: { import: job({ status: 'failed', message: 'the LinkedIn account is signed out, reconnect it and try again' }) } },
    })
    await u.type(screen.getByLabelText('LinkedIn people search URL'), 'https://www.linkedin.com/search/results/people/?keywords=x')
    await u.click(screen.getByRole('button', { name: 'Import from search' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/the search failed: the linkedin account is signed out/i)
    expect(screen.getByRole('button', { name: 'Import from search' })).toBeEnabled() // can try again
  })

  it('explains that searches only run during working hours, in the reader\'s time', async () => {
    const { u } = await openSearch({
      'POST /api/leads/import/search': apiError(409, 'outside_working_hours', "searches run during the account's working hours; next window opens 2026-11-09T04:00:00Z"),
    })
    await u.type(screen.getByLabelText('LinkedIn people search URL'), 'https://www.linkedin.com/search/results/people/?keywords=x')
    await u.click(screen.getByRole('button', { name: 'Import from search' }))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/only run during the account's working hours/i)
    expect(alert).toHaveTextContent(/next window opens/i)
    expect(alert).not.toHaveTextContent('2026-11-09T04:00:00Z') // formatted for people, not a raw timestamp
  })

  it('reports that another search is already running', async () => {
    const { u } = await openSearch({
      'POST /api/leads/import/search': apiError(409, 'import_in_progress', 'a search import is already running for this account'),
    })
    await u.type(screen.getByLabelText('LinkedIn people search URL'), 'https://www.linkedin.com/search/results/people/?keywords=x')
    await u.click(screen.getByRole('button', { name: 'Import from search' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/already running/i)
  })

  it('needs a URL and a sensible number before starting', async () => {
    const { u } = await openSearch()
    const start = screen.getByRole('button', { name: 'Import from search' })
    expect(start).toBeDisabled()
    await u.type(screen.getByLabelText('LinkedIn people search URL'), 'https://www.linkedin.com/search/results/people/?keywords=x')
    expect(start).toBeEnabled()
    fireEvent.change(screen.getByLabelText('How many people'), { target: { value: '0' } })
    expect(start).toBeDisabled()
    fireEvent.change(screen.getByLabelText('How many people'), { target: { value: '101' } })
    expect(start).toBeDisabled()
    fireEvent.change(screen.getByLabelText('How many people'), { target: { value: '100' } })
    expect(start).toBeEnabled()
  })

  it('lets a person with several accounts choose which one searches', async () => {
    const { api, u } = await openSearch({
      'GET /api/accounts': { body: { accounts: [account({ id: 1, name: 'Ada' }), account({ id: 2, name: 'Grace' })] } },
      'POST /api/leads/import/search': { status: 202, body: { import: job({ account_id: 2 }) } },
    })
    await u.selectOptions(await screen.findByLabelText('LinkedIn account'), 'Grace')
    await u.type(screen.getByLabelText('LinkedIn people search URL'), 'https://www.linkedin.com/search/results/people/?keywords=x')
    await u.click(screen.getByRole('button', { name: 'Import from search' }))
    await waitFor(() => expect(api.called('POST', '/api/leads/import/search')).toHaveLength(1))
    expect(api.called('POST', '/api/leads/import/search')[0].body.account_id).toBe(2)
  })
})
