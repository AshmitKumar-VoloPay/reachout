// Small typed fetch wrapper. Session cookies are HttpOnly and handled by the browser, so the
// client never sees a token: it only sends credentials and reads JSON.

export class ApiError extends Error {
  readonly status: number
  /** Stable machine-readable code from the server, e.g. "email_taken", or "network". */
  readonly code: string

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

interface ApiConfig {
  baseUrl: string
  fetch: typeof fetch
  /** Called when a signed-in request comes back 401, so the app can return to the login page. */
  onUnauthorized?: () => void
}

const config: ApiConfig = {
  baseUrl: import.meta.env.VITE_API_BASE ?? '',
  fetch: (...args) => globalThis.fetch(...args),
}

/** Change the base URL or fetch implementation (tests, live checks). */
export function configureApi(partial: Partial<ApiConfig>) {
  Object.assign(config, partial)
}

// Paths where a 401 is an answer ("wrong password"), not a lost session.
const authPaths = ['/api/auth/login', '/api/auth/signup']

export type Query = Record<string, string | number | boolean | undefined>

function withQuery(path: string, query?: Query): string {
  if (!query) return path
  const params = new URLSearchParams()
  for (const [k, v] of Object.entries(query)) {
    if (v !== undefined && v !== '') params.set(k, String(v))
  }
  const s = params.toString()
  return s ? `${path}?${s}` : path
}

export async function request<T>(
  method: 'GET' | 'POST' | 'PUT' | 'DELETE',
  path: string,
  options: { body?: unknown; query?: Query; signal?: AbortSignal } = {},
): Promise<T> {
  let res: Response
  try {
    res = await config.fetch(config.baseUrl + withQuery(path, options.query), {
      method,
      credentials: 'include',
      headers: options.body === undefined ? undefined : { 'Content-Type': 'application/json' },
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
      signal: options.signal,
    })
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') throw err
    throw new ApiError(0, 'network', 'Cannot reach the server. Check your connection and try again.')
  }

  if (res.status === 204) return undefined as T

  let data: unknown
  try {
    data = await res.json()
  } catch {
    data = undefined
  }

  if (!res.ok) {
    const e = (data as { error?: { code?: string; message?: string } } | undefined)?.error
    if (res.status === 401 && !authPaths.includes(path)) config.onUnauthorized?.()
    throw new ApiError(res.status, e?.code ?? 'unknown', e?.message ?? `Request failed (${res.status})`)
  }
  return data as T
}
