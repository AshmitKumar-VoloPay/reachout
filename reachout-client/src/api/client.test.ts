import { describe, expect, it, vi } from 'vitest'
import { configureApi, request, ApiError } from './client'
import { mockApi, apiError } from '../test/mockApi'

describe('saveSchedule', () => {
  it('sends only the editable fields, even when given a whole schedule view', async () => {
    const { api } = await import('./endpoints')
    const mock = mockApi({ 'PUT /api/accounts/:id/schedule': { body: {} } })
    await api.accounts.saveSchedule(1, {
      timezone: 'UTC', work_days: [1], start: '09:00', end: '17:00', holidays: [], daily_connect_cap: 1, daily_message_cap: 2,
      open_now: true, next_send_window: '2026-11-09T04:00:00Z',
    } as never)
    expect(Object.keys(mock.called('PUT', '/api/accounts/1/schedule')[0].body).sort()).toEqual(
      ['daily_connect_cap', 'daily_message_cap', 'end', 'holidays', 'start', 'timezone', 'work_days'],
    )
  })
})

describe('request', () => {
  it('sends JSON with credentials and parses the answer', async () => {
    const api = mockApi({ 'POST /api/things': (req) => ({ body: { got: req.body } }) })
    const out = await request<{ got: unknown }>('POST', '/api/things', { body: { a: 1 } })
    expect(out).toEqual({ got: { a: 1 } })
    const [url, init] = api.fetchMock.mock.calls[0] as [string, RequestInit]
    expect(url).toBe('/api/things')
    expect(init.credentials).toBe('include')
    expect(init.headers).toEqual({ 'Content-Type': 'application/json' })
  })

  it('does not set a content type when there is no body', async () => {
    const api = mockApi({ 'GET /api/x': { body: {} } })
    await request('GET', '/api/x')
    expect((api.fetchMock.mock.calls[0][1] as RequestInit).headers).toBeUndefined()
  })

  it('builds the query string and leaves out empty values', async () => {
    const api = mockApi({ 'GET /api/leads': { body: {} } })
    await request('GET', '/api/leads', { query: { q: 'cto', limit: 50, offset: 0, empty: '', missing: undefined } })
    expect(api.fetchMock.mock.calls[0][0]).toBe('/api/leads?q=cto&limit=50&offset=0')
    await request('GET', '/api/leads', { query: { q: '' } })
    expect(api.fetchMock.mock.calls[1][0]).toBe('/api/leads')
  })

  it('escapes query values', async () => {
    const api = mockApi({ 'GET /api/leads': { body: {} } })
    await request('GET', '/api/leads', { query: { q: 'a&b=c d' } })
    expect(api.fetchMock.mock.calls[0][0]).toBe('/api/leads?q=a%26b%3Dc+d')
  })

  it('returns undefined for an empty 204 answer', async () => {
    mockApi({ 'POST /api/auth/logout': { status: 204 } })
    expect(await request('POST', '/api/auth/logout')).toBeUndefined()
  })

  it('turns an error answer into an ApiError with its code and message', async () => {
    mockApi({ 'GET /api/x': apiError(409, 'connection_busy', 'Another connection is in progress') })
    const err = await request('GET', '/api/x').catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ status: 409, code: 'connection_busy', message: 'Another connection is in progress' })
  })

  it('copes with an error that is not JSON', async () => {
    configureApi({ fetch: (async () => new Response('<html>bad gateway</html>', { status: 502 })) as typeof fetch })
    const err = await request('GET', '/api/x').catch((e) => e)
    expect(err).toMatchObject({ status: 502, code: 'unknown', message: 'Request failed (502)' })
  })

  it('reports an unreachable server with a code the screens can recognise', async () => {
    configureApi({ fetch: (async () => { throw new TypeError('fetch failed') }) as typeof fetch })
    const err = (await request('GET', '/api/x').catch((e) => e)) as ApiError
    expect(err).toMatchObject({ status: 0, code: 'network' })
    expect(err.message).toMatch(/cannot reach the server/i)
  })

  it('lets an aborted request through unchanged', async () => {
    configureApi({ fetch: (async () => { throw new DOMException('aborted', 'AbortError') }) as typeof fetch })
    const err = await request('GET', '/api/x').catch((e) => e)
    expect(err).toBeInstanceOf(DOMException)
  })

  it('tells the app when a signed-in request loses its session, but not for a wrong password', async () => {
    mockApi({
      'GET /api/leads': apiError(401, 'unauthorized', 'sign in required'),
      'POST /api/auth/login': apiError(401, 'invalid_credentials', 'invalid email or password'),
      'POST /api/auth/signup': apiError(429, 'too_many_attempts', 'slow down'),
    })
    const onUnauthorized = vi.fn()
    configureApi({ onUnauthorized })

    await request('GET', '/api/leads').catch(() => {})
    expect(onUnauthorized).toHaveBeenCalledTimes(1)

    await request('POST', '/api/auth/login', { body: {} }).catch(() => {})
    await request('POST', '/api/auth/signup', { body: {} }).catch(() => {})
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('puts the configured base URL in front of every path', async () => {
    const api = mockApi({ 'GET /api/x': { body: {} } })
    configureApi({ baseUrl: 'https://api.example.com' })
    await request('GET', '/api/x')
    expect(api.fetchMock.mock.calls[0][0]).toBe('https://api.example.com/api/x')
  })
})
