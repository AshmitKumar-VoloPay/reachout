import { ApiError, request } from './client'
import type {
  Account,
  BulkDeleteResult,
  LeadsPage,
  PasteImportResult,
  ScheduleSettings,
  ScheduleView,
  SearchImport,
  User,
} from './types'

export const api = {
  auth: {
    /** The signed-in user, or null when there is no valid session. */
    async me(): Promise<User | null> {
      try {
        return (await request<{ user: User }>('GET', '/api/auth/me')).user
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) return null
        throw err
      }
    },
    async signup(email: string, password: string): Promise<User> {
      return (await request<{ user: User }>('POST', '/api/auth/signup', { body: { email, password } })).user
    },
    async login(email: string, password: string): Promise<User> {
      return (await request<{ user: User }>('POST', '/api/auth/login', { body: { email, password } })).user
    },
    logout: () => request<void>('POST', '/api/auth/logout'),
  },

  accounts: {
    async list(): Promise<Account[]> {
      return (await request<{ accounts: Account[] }>('GET', '/api/accounts')).accounts
    },
    /** Starts a LinkedIn connection. The result carries `connection_link`, shown once. */
    async connect(timezone: string): Promise<Account> {
      return (await request<{ account: Account }>('POST', '/api/accounts/connect', { body: { timezone } })).account
    },
    /** Asks the server to check Linked API again; also finishes a pending connection. */
    async refresh(id: number): Promise<Account> {
      return (await request<{ account: Account }>('POST', `/api/accounts/${id}/refresh`)).account
    },
    async reconnect(id: number): Promise<string> {
      return (await request<{ reconnection_link: string }>('POST', `/api/accounts/${id}/reconnect`)).reconnection_link
    },
    schedule: (id: number) => request<ScheduleView>('GET', `/api/accounts/${id}/schedule`),
    // Only the editable fields are sent: the server rejects anything else (such as the
    // read-only `open_now` and `next_send_window` of a ScheduleView).
    saveSchedule: (id: number, s: ScheduleSettings) =>
      request<ScheduleView>('PUT', `/api/accounts/${id}/schedule`, {
        body: {
          timezone: s.timezone,
          work_days: s.work_days,
          start: s.start,
          end: s.end,
          holidays: s.holidays,
          daily_connect_cap: s.daily_connect_cap,
          daily_message_cap: s.daily_message_cap,
        },
      }),
  },

  leads: {
    list: (q: string, limit: number, offset: number) =>
      request<LeadsPage>('GET', '/api/leads', { query: { q, limit, offset } }),
    importUrls: (text: string) => request<PasteImportResult>('POST', '/api/leads/import/urls', { body: { text } }),
    async startSearch(accountId: number, searchUrl: string, limit: number): Promise<SearchImport> {
      const r = await request<{ import: SearchImport }>('POST', '/api/leads/import/search', {
        body: { account_id: accountId, search_url: searchUrl, limit },
      })
      return r.import
    },
    async getImport(id: number): Promise<SearchImport> {
      return (await request<{ import: SearchImport }>('GET', `/api/leads/imports/${id}`)).import
    },
    remove: (id: number) => request<void>('DELETE', `/api/leads/${id}`),
    bulkRemove: (ids: number[]) => request<BulkDeleteResult>('POST', '/api/leads/bulk-delete', { body: { ids } }),
  },
}
