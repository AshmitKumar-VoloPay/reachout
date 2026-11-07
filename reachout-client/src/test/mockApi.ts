import { vi } from 'vitest'
import { configureApi } from '../api/client'
import type { Account, Lead, LeadsPage, ScheduleView, User } from '../api/types'

export interface Req {
  method: string
  path: string
  query: URLSearchParams
  body: any // tests inspect request bodies freely
}
export interface Res {
  status?: number
  body?: unknown
}
export type Handler = Res | ((req: Req, params: Record<string, string>) => Res | Promise<Res>)

interface Route {
  method: string
  pattern: RegExp
  names: string[]
  handler: Handler
}

function compile(key: string): Omit<Route, 'handler'> {
  const [method, path] = key.split(' ')
  const names: string[] = []
  const src = path.replace(/:([a-zA-Z]+)/g, (_m, n: string) => {
    names.push(n)
    return '([^/]+)'
  })
  return { method, pattern: new RegExp(`^${src}$`), names }
}

/** Replaces fetch with a fake server. Keys look like "GET /api/leads" or "DELETE /api/leads/:id". */
export function mockApi(routes: Record<string, Handler> = {}) {
  const table: Route[] = Object.entries(routes).map(([k, handler]) => ({ ...compile(k), handler }))
  const calls: Req[] = []
  const unhandled: string[] = []

  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = (init?.method ?? 'GET').toUpperCase()
    const body = init?.body ? JSON.parse(String(init.body)) : undefined
    const req: Req = { method, path: url.pathname, query: url.searchParams, body }
    calls.push(req)

    // Later definitions win, so a test can override a default route.
    for (const r of [...table].reverse()) {
      if (r.method !== method) continue
      const m = r.pattern.exec(url.pathname)
      if (!m) continue
      const params = Object.fromEntries(r.names.map((n, i) => [n, m[i + 1]]))
      const res = typeof r.handler === 'function' ? await r.handler(req, params) : r.handler
      const status = res.status ?? 200
      if (status === 204) return new Response(null, { status })
      return new Response(JSON.stringify(res.body ?? {}), { status, headers: { 'Content-Type': 'application/json' } })
    }
    unhandled.push(`${method} ${url.pathname}`)
    return new Response(JSON.stringify({ error: { code: 'unmocked', message: `unmocked ${method} ${url.pathname}` } }), { status: 500 })
  })
  configureApi({ fetch: fetchMock as unknown as typeof fetch, baseUrl: '' })

  return {
    calls,
    unhandled,
    fetchMock,
    /** Add or replace a route. */
    on(key: string, handler: Handler) {
      table.push({ ...compile(key), handler })
    },
    /** Requests made to a route, e.g. called("POST", "/api/leads/import/urls"). */
    called(method: string, path: string) {
      return calls.filter((c) => c.method === method && c.path === path)
    },
  }
}

/** A handler that gives each response in turn and repeats the last one. */
export function sequence(...responses: Res[]): Handler {
  let i = 0
  return () => responses[Math.min(i++, responses.length - 1)]
}

export function apiError(status: number, code: string, message: string): Res {
  return { status, body: { error: { code, message } } }
}

// ---- fixtures ----

export const user: User = { id: 1, email: 'ada@example.com', created_at: '2026-11-01T10:00:00Z' }

export function account(over: Partial<Account> = {}): Account {
  return {
    id: 1,
    name: 'Ada Lovelace',
    profile_url: 'https://www.linkedin.com/in/ada',
    headline: 'Founder at Analytical Engines',
    avatar_url: '',
    status: 'connected',
    connected_at: '2026-11-02T10:00:00Z',
    created_at: '2026-11-02T09:00:00Z',
    ...over,
  }
}

export function schedule(over: Partial<ScheduleView> = {}): ScheduleView {
  return {
    timezone: 'Asia/Kolkata',
    work_days: [1, 2, 3, 4, 5],
    start: '09:30',
    end: '17:30',
    holidays: [],
    daily_connect_cap: 5,
    daily_message_cap: 10,
    open_now: false,
    next_send_window: '2026-11-09T04:00:00Z', // Monday 09:30 in Kolkata
    ...over,
  }
}

export function lead(id: number, over: Partial<Lead> = {}): Lead {
  return {
    id,
    profile_url: `https://www.linkedin.com/in/person-${id}`,
    name: `Person ${id}`,
    headline: `Headline ${id}`,
    company: '',
    created_at: '2026-11-03T10:00:00Z',
    ...over,
  }
}

export function leadsPage(leads: Lead[], total = leads.length, offset = 0): LeadsPage {
  return { leads, total, limit: 50, offset }
}

/** Routes for a signed-in user with nothing else set up. */
export function signedIn(): Record<string, Handler> {
  return {
    'GET /api/auth/me': { body: { user } },
    'GET /api/accounts': { body: { accounts: [] } },
    'GET /api/leads': { body: leadsPage([]) },
  }
}
