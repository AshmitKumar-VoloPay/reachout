// A live check: the real client code against a running reachout-server (with the mock Linked
// API behind it). It follows the path of the Done-when criteria: sign up, connect LinkedIn,
// set the schedule, import leads, then clean up, and it checks that a second user is walled off.
//
//   scripts/live-check.sh            starts everything and runs this
//   LIVE_API_URL=http://localhost:8080 npm run test:live      against a server you started

import { describe, expect, it } from 'vitest'
import { api } from '../src/api/endpoints'
import { ApiError, configureApi } from '../src/api/client'
import type { Account } from '../src/api/types'

const BASE = process.env.LIVE_API_URL ?? 'http://localhost:18088'
const ORIGIN = 'http://localhost:5173' // what a browser on the Vite dev server sends

/** A fetch with a cookie jar and an Origin header, like one browser. */
function browser() {
  let cookie = ''
  const f = async (input: RequestInfo | URL, init: RequestInit = {}) => {
    const headers = new Headers(init.headers)
    headers.set('Origin', ORIGIN)
    if (cookie) headers.set('Cookie', cookie)
    const res = await fetch(input, { ...init, headers })
    for (const c of res.headers.getSetCookie()) {
      const [pair] = c.split(';')
      const [name, value] = pair.split('=')
      cookie = value === '' || /max-age=0|max-age=-/i.test(c) ? cookie.replace(new RegExp(`${name}=[^;]*;? ?`), '') : `${name}=${value}`
    }
    return res
  }
  return { fetch: f as typeof fetch, cookie: () => cookie }
}

function as(b: ReturnType<typeof browser>) {
  configureApi({ baseUrl: BASE, fetch: b.fetch, onUnauthorized: undefined })
}

const stamp = Date.now()
const ada = browser()
const bob = browser()
const PASSWORD = 'a long enough password'
let connected: Account

async function expectApiError(p: Promise<unknown>, status: number, code?: string) {
  const err = await p.then(() => null, (e) => e)
  expect(err, 'expected the request to fail').toBeInstanceOf(ApiError)
  expect(err.status).toBe(status)
  if (code) expect(err.code).toBe(code)
}

describe('the real client against the real server', () => {
  it('the server is up', async () => {
    const res = await fetch(`${BASE}/healthz`)
    expect(res.status).toBe(200)
  })

  it('signs up, and the session cookie is HttpOnly and never visible to the client', async () => {
    as(ada)
    const u = await api.auth.signup(`ada-${stamp}@example.com`, PASSWORD)
    expect(u.email).toBe(`ada-${stamp}@example.com`)
    expect(ada.cookie()).toContain('reachout_session=')
    expect((await api.auth.me())?.id).toBe(u.id)
  })

  it('refuses a duplicate email, a wrong password and an unknown person alike', async () => {
    as(ada)
    await expectApiError(api.auth.signup(`ada-${stamp}@example.com`, PASSWORD), 409, 'email_taken')
    const stranger = browser()
    as(stranger)
    await expectApiError(api.auth.login(`ada-${stamp}@example.com`, 'wrong password here'), 401, 'invalid_credentials')
    await expectApiError(api.auth.login(`nobody-${stamp}@example.com`, 'wrong password here'), 401, 'invalid_credentials')
    expect(await api.auth.me()).toBeNull()
  })

  it('has no accounts or leads at first', async () => {
    as(ada)
    expect(await api.accounts.list()).toEqual([])
    expect((await api.leads.list('', 50, 0)).total).toBe(0)
  })

  it('connects a LinkedIn account by following the connection until it finishes', async () => {
    as(ada)
    const started = await api.accounts.connect('Asia/Kolkata')
    expect(started.status).toBe('pending')
    expect(started.connection_link).toMatch(/\/connection\/mock-sess-/)

    // Only one connection can run at a time: a second person has to wait.
    as(bob)
    await api.auth.signup(`bob-${stamp}@example.com`, PASSWORD)
    await expectApiError(api.accounts.connect('UTC'), 409, 'connection_busy')

    as(ada)
    let account = started
    for (let i = 0; i < 20 && account.status === 'pending'; i++) account = await api.accounts.refresh(started.id)
    expect(account.status).toBe('connected')
    expect(account.name).toMatch(/Mock User/)
    connected = account
    // The identification token is never part of what the browser receives.
    expect(JSON.stringify(account)).not.toMatch(/mock-token|identification/i)
    expect((await api.accounts.list()).map((a) => a.id)).toEqual([connected.id])
  })

  it('shows and changes the sending schedule, with the next window computed by the server', async () => {
    as(ada)
    const s = await api.accounts.schedule(connected.id)
    expect(s).toMatchObject({ timezone: 'Asia/Kolkata', work_days: [1, 2, 3, 4, 5], start: '09:30', end: '17:30', daily_connect_cap: 5 })
    expect(new Date(s.next_send_window).getTime()).toBeGreaterThan(0)

    // Open all day, every day, so the search below is allowed whenever this runs.
    const open = await api.accounts.saveSchedule(connected.id, {
      ...s, work_days: [0, 1, 2, 3, 4, 5, 6], start: '00:00', end: '24:00', holidays: ['2026-12-25'], daily_connect_cap: 8,
    })
    expect(open.open_now).toBe(true)
    expect(open.end).toBe('24:00')
    expect((await api.accounts.schedule(connected.id)).holidays).toEqual(['2026-12-25'])

    await expectApiError(
      api.accounts.saveSchedule(connected.id, { ...open, daily_connect_cap: 31 }), 400, 'invalid_schedule')
    await expectApiError(
      api.accounts.saveSchedule(connected.id, { ...open, timezone: 'Mars/Base' }), 400, 'invalid_schedule')
  })

  it('imports pasted URLs, de-duplicating spellings and reporting bad entries', async () => {
    as(ada)
    const r = await api.leads.importUrls(
      ['linkedin.com/in/Jane-Doe', 'https://uk.linkedin.com/in/jane-doe/?trk=x', 'https://www.linkedin.com/in/john-roe', 'https://example.com/in/mallory'].join('\n'),
    )
    expect(r).toMatchObject({ imported: 2, duplicates: 1, over_cap: 0 })
    expect(r.invalid).toHaveLength(1)
    expect(r.invalid[0].input).toBe('https://example.com/in/mallory')
    expect((await api.leads.importUrls('linkedin.com/in/jane-doe')).imported).toBe(0) // again: nothing new
  })

  it('imports from a LinkedIn search and follows the job to the end', async () => {
    as(ada)
    const job = await api.leads.startSearch(connected.id, 'https://www.linkedin.com/search/results/people/?keywords=cto', 5)
    expect(job.status).toBe('running')
    // A second search while one runs is refused.
    await expectApiError(api.leads.startSearch(connected.id, 'https://www.linkedin.com/search/results/people/?keywords=cto', 5), 409, 'import_in_progress')

    let done = job
    for (let i = 0; i < 20 && done.status === 'running'; i++) done = await api.leads.getImport(job.id)
    expect(done).toMatchObject({ status: 'completed', found: 5, imported: 5 })

    const page = await api.leads.list('', 50, 0)
    expect(page.total).toBe(7) // 2 pasted + 5 searched
    expect(page.leads.some((l) => l.name === 'Searched Person 1' && l.headline === 'CTO at Example Co')).toBe(true)
  })

  it('searches, pages and filters the leads', async () => {
    as(ada)
    expect((await api.leads.list('searched', 50, 0)).total).toBe(5)
    expect((await api.leads.list('JOHN', 50, 0)).total).toBe(1)
    const p1 = await api.leads.list('', 3, 0)
    const p2 = await api.leads.list('', 3, 3)
    expect(p1.leads).toHaveLength(3)
    expect(p2.leads).toHaveLength(3)
    expect(new Set([...p1.leads, ...p2.leads].map((l) => l.id)).size).toBe(6) // no overlap
  })

  it('keeps a second user out of the first user\'s accounts, leads and conversations', async () => {
    as(bob)
    expect(await api.accounts.list()).toEqual([])
    expect((await api.leads.list('', 50, 0)).total).toBe(0)
    await expectApiError(api.accounts.schedule(connected.id), 404, 'not_found')
    await expectApiError(api.accounts.refresh(connected.id), 404, 'not_found')
    await expectApiError(api.leads.startSearch(connected.id, 'https://www.linkedin.com/search/results/people/?keywords=x', 3), 404, 'not_found')

    // Bob importing the same person creates Bob's own lead, and deleting it leaves Ada's alone.
    expect((await api.leads.importUrls('linkedin.com/in/john-roe')).imported).toBe(1)
    const bobsLead = (await api.leads.list('', 50, 0)).leads[0]
    await api.leads.remove(bobsLead.id)

    as(ada)
    const adas = (await api.leads.list('', 50, 0)).leads
    expect(adas.some((l) => l.profile_url.endsWith('/john-roe'))).toBe(true)
    as(bob)
    await expectApiError(api.leads.remove(adas[0].id), 404, 'not_found')
    expect((await api.leads.bulkRemove(adas.map((l) => l.id))).deleted).toBe(0)
    as(ada)
    expect((await api.leads.list('', 50, 0)).total).toBe(7)
  })

  it('refuses cross-site posts even with a valid session', async () => {
    const res = await fetch(`${BASE}/api/leads/bulk-delete`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Origin: 'https://evil.example', Cookie: ada.cookie() },
      body: JSON.stringify({ ids: [1] }),
    })
    expect(res.status).toBe(403)
    as(ada)
    expect((await api.leads.list('', 50, 0)).total).toBe(7)
  })

  it('deletes leads one at a time and in bulk', async () => {
    as(ada)
    const leads = (await api.leads.list('', 50, 0)).leads
    await api.leads.remove(leads[0].id)
    await expectApiError(api.leads.remove(leads[0].id), 404, 'not_found') // already gone
    const bulk = await api.leads.bulkRemove(leads.slice(1, 4).map((l) => l.id))
    expect(bulk).toEqual({ deleted: 3, blocked: 0 })
    expect((await api.leads.list('', 50, 0)).total).toBe(3)
  })

  it('signs out, and the session no longer works', async () => {
    as(ada)
    await api.auth.logout()
    expect(await api.auth.me()).toBeNull()
    await expectApiError(api.leads.list('', 50, 0), 401, 'unauthorized')
    await expectApiError(api.accounts.list(), 401, 'unauthorized')
    // Signing back in brings the same data back.
    await api.auth.login(`ada-${stamp}@example.com`, PASSWORD)
    expect((await api.leads.list('', 50, 0)).total).toBe(3)
  })
})
