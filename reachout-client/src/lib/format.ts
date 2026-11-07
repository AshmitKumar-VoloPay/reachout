import { ApiError } from '../api/client'
import type { AccountStatus } from '../api/types'

export function formatDate(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

/** "Mon 9 Nov, 09:30 GMT+5:30" in the given IANA timezone (falls back to the browser's). */
export function formatMoment(iso: string, timeZone?: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const opts: Intl.DateTimeFormatOptions = {
    weekday: 'short',
    day: 'numeric',
    month: 'short',
    hour: '2-digit',
    minute: '2-digit',
    timeZoneName: 'short',
  }
  try {
    return new Intl.DateTimeFormat(undefined, { ...opts, timeZone }).format(d)
  } catch {
    return new Intl.DateTimeFormat(undefined, opts).format(d)
  }
}

export interface StatusInfo {
  label: string
  tone: 'good' | 'warn' | 'bad' | 'neutral'
}

export function statusInfo(status: AccountStatus): StatusInfo {
  switch (status) {
    case 'connected':
      return { label: 'Connected', tone: 'good' }
    case 'pending':
      return { label: 'Connecting…', tone: 'neutral' }
    case 'reconnection_required':
      return { label: 'Needs reconnecting', tone: 'warn' }
    case 'frozen':
      return { label: 'Frozen', tone: 'warn' }
    case 'disconnected':
      return { label: 'Disconnected', tone: 'bad' }
    case 'error':
      return { label: 'Connection failed', tone: 'bad' }
  }
}

const isoRe = /\d{4}-\d{2}-\d{2}T[\d:.]+(?:Z|[+-]\d{2}:\d{2})/

/** A message a person can act on. Server texts are already plain; a few are improved here. */
export function errorMessage(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Something went wrong. Please try again.'
  switch (err.code) {
    case 'invalid_credentials':
      return 'Email or password is incorrect.'
    case 'too_many_attempts':
      return 'Too many attempts. Wait a few minutes and try again.'
    case 'email_taken':
      return 'An account with this email already exists. Try signing in instead.'
    case 'outside_working_hours': {
      // "…next window opens 2026-11-09T09:30:00Z": show the time in the reader's terms.
      const m = isoRe.exec(err.message)
      return m
        ? `Searches only run during the account's working hours. The next window opens ${formatMoment(m[0])}.`
        : err.message
    }
    case 'internal':
      return 'The server had a problem. Please try again in a moment.'
  }
  return err.message
}

/** Weekdays in the order a work week is shown (Monday first). Values match the server (0 = Sunday). */
export const WEEKDAYS: { value: number; short: string; long: string }[] = [
  { value: 1, short: 'Mon', long: 'Monday' },
  { value: 2, short: 'Tue', long: 'Tuesday' },
  { value: 3, short: 'Wed', long: 'Wednesday' },
  { value: 4, short: 'Thu', long: 'Thursday' },
  { value: 5, short: 'Fri', long: 'Friday' },
  { value: 6, short: 'Sat', long: 'Saturday' },
  { value: 0, short: 'Sun', long: 'Sunday' },
]

// Browsers list some zones only under their older names (Asia/Calcutta, Europe/Kiev). People
// look for the current names, so these are added when the browser accepts them.
const modernNames = ['Asia/Kolkata', 'Asia/Kathmandu', 'Asia/Ho_Chi_Minh', 'Asia/Yangon', 'Europe/Kyiv', 'America/Nuuk']

function isKnownZone(z: string): boolean {
  try {
    new Intl.DateTimeFormat(undefined, { timeZone: z })
    return true
  } catch {
    return false
  }
}

/** All IANA timezones the browser knows, with `extra` ensured to be present. */
export function timezones(...extra: string[]): string[] {
  let list: string[] = []
  try {
    list = Intl.supportedValuesOf('timeZone')
  } catch {
    list = []
  }
  const set = new Set([...list, ...modernNames.filter(isKnownZone), ...extra.filter(Boolean)])
  if (!set.has('UTC')) set.add('UTC')
  return [...set].sort()
}

export function browserTimezone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
  } catch {
    return 'UTC'
  }
}
