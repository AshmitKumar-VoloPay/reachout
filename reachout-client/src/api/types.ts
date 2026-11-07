// Shapes returned by reachout-server. Field names match its JSON exactly.

export interface User {
  id: number
  email: string
  created_at: string
}

export type AccountStatus =
  | 'pending'
  | 'connected'
  | 'frozen'
  | 'reconnection_required'
  | 'disconnected'
  | 'error'

export interface Account {
  id: number
  name: string
  profile_url: string
  headline: string
  avatar_url: string
  status: AccountStatus
  status_detail?: string
  connected_at?: string
  /** Only present in the response that starts a connection. */
  connection_link?: string
  reconnection_link?: string
  created_at: string
}

export interface ScheduleSettings {
  timezone: string
  /** 0 = Sunday ... 6 = Saturday */
  work_days: number[]
  /** "HH:MM" local time the window opens */
  start: string
  /** "HH:MM" local time the window closes ("24:00" means end of day) */
  end: string
  /** "YYYY-MM-DD" local dates */
  holidays: string[]
  daily_connect_cap: number
  daily_message_cap: number
}

export interface ScheduleView extends ScheduleSettings {
  open_now: boolean
  /** Earliest moment sending is allowed (ISO time). */
  next_send_window: string
}

export interface Lead {
  id: number
  profile_url: string
  name: string
  headline: string
  company: string
  created_at: string
}

export interface LeadsPage {
  leads: Lead[]
  total: number
  limit: number
  offset: number
}

export interface InvalidUrl {
  input: string
  reason: string
}

export interface PasteImportResult {
  imported: number
  duplicates: number
  over_cap: number
  invalid: InvalidUrl[]
}

export type ImportStatus = 'running' | 'completed' | 'failed'

export interface SearchImport {
  id: number
  account_id: number
  search_url: string
  requested: number
  status: ImportStatus
  found: number
  imported: number
  duplicates: number
  invalid: number
  over_cap: number
  message?: string
  /** While running: queued | outsideWorkingHours */
  pending_reason?: string
  created_at: string
  finished_at?: string
}

export interface BulkDeleteResult {
  deleted: number
  blocked: number
}
