import type { ScheduleSettings } from '../api/types'

// Mirrors the limits the server enforces. The server stays the authority; these only give
// instant feedback.
export const MAX_CONNECT = 30
export const MAX_MESSAGE = 100

/** Problems with the settings, by field. Empty means they can be saved. */
export function validate(s: ScheduleSettings): Record<string, string> {
  const errors: Record<string, string> = {}
  if (s.work_days.length === 0) errors.days = 'Pick at least one working day.'
  if (!s.start || !s.end) errors.window = 'Set both a start and an end time.'
  else if (s.start >= s.end) errors.window = 'The window must end after it starts.'
  const cc = s.daily_connect_cap
  if (!Number.isInteger(cc) || cc < 0 || cc > MAX_CONNECT) errors.connect = `Enter a whole number from 0 to ${MAX_CONNECT}.`
  const mc = s.daily_message_cap
  if (!Number.isInteger(mc) || mc < 0 || mc > MAX_MESSAGE) errors.message = `Enter a whole number from 0 to ${MAX_MESSAGE}.`
  return errors
}
