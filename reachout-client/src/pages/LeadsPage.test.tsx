import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { apiError, lead, leadsPage, mockApi, signedIn } from '../test/mockApi'
import { renderApp } from '../test/render'

const many = (from: number, n: number) => Array.from({ length: n }, (_, i) => lead(from + i))

function open(routes: Parameters<typeof mockApi>[0] = {}) {
  const api = mockApi({ ...signedIn(), ...routes })
  renderApp('/leads')
  return { api, u: userEvent.setup() }
}

describe('the leads table', () => {
  it('invites a new user to import their first leads', async () => {
    open()
    expect(await screen.findByText('No leads yet')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'connect a LinkedIn account' })).toHaveAttribute('href', '/accounts')
    expect(screen.getByText('0 leads')).toBeInTheDocument()
  })

  it('lists leads with a safe link to each profile', async () => {
    open({
      'GET /api/leads': {
        body: leadsPage([
          lead(1, { name: 'Jane Doe', headline: 'CTO at Acme', company: 'Acme' }),
          lead(2, { name: '', headline: '' }), // a pasted URL that has not been enriched
        ]),
      },
    })
    const jane = await screen.findByRole('link', { name: 'Jane Doe' })
    expect(jane).toHaveAttribute('href', 'https://www.linkedin.com/in/person-1')
    expect(jane).toHaveAttribute('target', '_blank')
    expect(jane).toHaveAttribute('rel', expect.stringMatching(/noopener/))
    const row = jane.closest('tr') as HTMLElement
    expect(within(row).getByText('CTO at Acme')).toBeInTheDocument()
    expect(within(row).getAllByText('Acme').length).toBeGreaterThan(0)
    // With no name, the profile path stands in.
    expect(screen.getByRole('link', { name: '/in/person-2' })).toBeInTheDocument()
    expect(screen.getByText('2 leads')).toBeInTheDocument()
  })

  it('shows data as text, never as markup', async () => {
    open({ 'GET /api/leads': { body: leadsPage([lead(1, { name: '<img src=x onerror=alert(1)>', headline: '<b>bold</b>' })]) } })
    expect(await screen.findByText('<img src=x onerror=alert(1)>')).toBeInTheDocument()
    expect(document.querySelector('img')).toBeNull()
    expect(document.querySelector('td b')).toBeNull()
  })

  it('shows a load failure', async () => {
    open({ 'GET /api/leads': apiError(500, 'internal', 'internal server error') })
    expect(await screen.findByRole('alert')).toHaveTextContent(/server had a problem/i)
  })
})

describe('searching', () => {
  it('searches after typing stops, from the first page', async () => {
    const { api, u } = open({ 'GET /api/leads': (req) => ({ body: leadsPage(req.query.get('q') ? [lead(9, { name: 'Match' })] : many(1, 3), req.query.get('q') ? 1 : 3) }) })
    await screen.findByText('Person 1')
    await u.type(screen.getByRole('searchbox', { name: 'Search leads' }), 'cto')
    expect(await screen.findByText('Match')).toBeInTheDocument()
    expect(screen.queryByText('Person 1')).not.toBeInTheDocument()
    const last = api.called('GET', '/api/leads').at(-1)!
    expect(last.query.get('q')).toBe('cto')
    expect(last.query.get('offset')).toBe('0')
    // Not one request per keystroke.
    expect(api.called('GET', '/api/leads').filter((c) => c.query.get('q')).every((c) => c.query.get('q') === 'cto')).toBe(true)
  })

  it('says when nothing matches', async () => {
    const { u } = open({ 'GET /api/leads': (req) => ({ body: leadsPage(req.query.get('q') ? [] : many(1, 2)) }) })
    await screen.findByText('Person 1')
    await u.type(screen.getByRole('searchbox'), 'zzz')
    expect(await screen.findByText('No leads match your search')).toBeInTheDocument()
    expect(screen.queryByText('No leads yet')).not.toBeInTheDocument()
  })
})

describe('paging', () => {
  const pages = (req: { query: URLSearchParams }) => {
    const offset = Number(req.query.get('offset') ?? 0)
    return { body: leadsPage(many(offset + 1, Math.min(50, 120 - offset)), 120, offset) }
  }

  it('moves through the pages and says where you are', async () => {
    const { api, u } = open({ 'GET /api/leads': pages })
    expect(await screen.findByText('Showing 1–50 of 120')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled()

    await u.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByText('Showing 51–100 of 120')).toBeInTheDocument()
    expect(api.called('GET', '/api/leads').at(-1)!.query.get('offset')).toBe('50')

    await u.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByText('Showing 101–120 of 120')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Next' })).toBeDisabled()

    await u.click(screen.getByRole('button', { name: 'Previous' }))
    expect(await screen.findByText('Showing 51–100 of 120')).toBeInTheDocument()
  })

  it('drops the selection when the page changes', async () => {
    const { u } = open({ 'GET /api/leads': pages })
    await screen.findByText('Showing 1–50 of 120')
    await u.click(screen.getByRole('checkbox', { name: 'Select Person 1' }))
    expect(screen.getByRole('button', { name: 'Delete selected (1)' })).toBeInTheDocument()
    await u.click(screen.getByRole('button', { name: 'Next' }))
    await screen.findByText('Showing 51–100 of 120')
    expect(screen.queryByRole('button', { name: /delete selected/i })).not.toBeInTheDocument()
  })
})

describe('deleting', () => {
  it('asks before deleting one lead, then deletes it', async () => {
    let remaining = [lead(1, { name: 'Jane' }), lead(2, { name: 'John' })]
    const { api, u } = open({
      'GET /api/leads': () => ({ body: leadsPage(remaining) }),
      'DELETE /api/leads/:id': (_r, p) => {
        remaining = remaining.filter((l) => String(l.id) !== p.id)
        return { status: 204 }
      },
    })
    await screen.findByText('Jane')
    await u.click(screen.getByRole('button', { name: 'Delete Jane' }))
    expect(api.called('DELETE', '/api/leads/1')).toHaveLength(0) // not yet
    await u.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(screen.getByRole('button', { name: 'Delete Jane' })).toBeInTheDocument() // asking was cancelled

    await u.click(screen.getByRole('button', { name: 'Delete Jane' }))
    await u.click(screen.getByRole('button', { name: 'Confirm delete Jane' }))
    expect(await screen.findByText('Lead deleted.')).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByText('Jane')).not.toBeInTheDocument())
    expect(api.called('DELETE', '/api/leads/1')).toHaveLength(1)
    expect(screen.getByText('John')).toBeInTheDocument()
  })

  it('explains why a lead in a running campaign cannot be deleted', async () => {
    const { u } = open({
      'GET /api/leads': { body: leadsPage([lead(1, { name: 'Jane' })]) },
      'DELETE /api/leads/:id': apiError(409, 'lead_in_active_campaign', 'lead is in a running or paused campaign; remove it from the campaign or stop the campaign first'),
    })
    await u.click(await screen.findByRole('button', { name: 'Delete Jane' }))
    await u.click(screen.getByRole('button', { name: 'Confirm delete Jane' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/running or paused campaign/i)
    expect(screen.getByText('Jane')).toBeInTheDocument()
  })

  it('deletes the selected leads after confirmation and reports any that were kept', async () => {
    const { api, u } = open({
      'GET /api/leads': { body: leadsPage(many(1, 4)) },
      'POST /api/leads/bulk-delete': { body: { deleted: 2, blocked: 1 } },
    })
    await screen.findByText('Person 1')
    await u.click(screen.getByRole('checkbox', { name: 'Select Person 1' }))
    await u.click(screen.getByRole('checkbox', { name: 'Select Person 3' }))
    await u.click(screen.getByRole('checkbox', { name: 'Select Person 4' }))
    await u.click(screen.getByRole('button', { name: 'Delete selected (3)' }))
    expect(screen.getByText(/delete 3 leads\?/i)).toBeInTheDocument()
    expect(api.called('POST', '/api/leads/bulk-delete')).toHaveLength(0) // asked first
    await u.click(screen.getByRole('button', { name: 'Yes, delete' }))

    const msg = await screen.findByText(/deleted 2 leads/i)
    expect(msg).toHaveTextContent('1 lead is in a running or paused campaign and was kept.')
    expect(api.called('POST', '/api/leads/bulk-delete')[0].body).toEqual({ ids: [1, 3, 4] })
    expect(screen.queryByRole('button', { name: /delete selected/i })).not.toBeInTheDocument() // selection cleared
  })

  it('selects everything on the page at once', async () => {
    const { u } = open({ 'GET /api/leads': { body: leadsPage(many(1, 3)) } })
    await screen.findByText('Person 1')
    await u.click(screen.getByRole('checkbox', { name: 'Select all leads on this page' }))
    expect(screen.getByRole('button', { name: 'Delete selected (3)' })).toBeInTheDocument()
    await u.click(screen.getByRole('checkbox', { name: 'Select all leads on this page' }))
    expect(screen.queryByRole('button', { name: /delete selected/i })).not.toBeInTheDocument()
  })

  it('steps back a page when the last lead on a later page is deleted', async () => {
    let total = 51
    const { api, u } = open({
      'GET /api/leads': (req) => {
        const offset = Number(req.query.get('offset') ?? 0)
        return { body: leadsPage(offset === 0 ? many(1, Math.min(50, total)) : total > 50 ? [lead(51)] : [], total, offset) }
      },
      'DELETE /api/leads/:id': () => {
        total = 50
        return { status: 204 }
      },
    })
    await screen.findByText('Showing 1–50 of 51')
    await u.click(screen.getByRole('button', { name: 'Next' }))
    await screen.findByText('Showing 51–51 of 51')
    await u.click(screen.getByRole('button', { name: 'Delete Person 51' }))
    await u.click(screen.getByRole('button', { name: 'Confirm delete Person 51' }))
    expect(await screen.findByText('Showing 1–50 of 50')).toBeInTheDocument() // not an empty page 2
    expect(api.called('GET', '/api/leads').at(-1)!.query.get('offset')).toBe('0')
  })
})
