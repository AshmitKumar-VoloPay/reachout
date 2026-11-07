import { describe, expect, it } from 'vitest'
import { ApiError } from '../api/client'
import type { AccountStatus } from '../api/types'
import { browserTimezone, errorMessage, formatDate, formatMoment, statusInfo, timezones, WEEKDAYS } from './format'

describe('errorMessage', () => {
  it('gives plain wording for the errors people meet', () => {
    expect(errorMessage(new ApiError(401, 'invalid_credentials', 'invalid email or password'))).toBe('Email or password is incorrect.')
    expect(errorMessage(new ApiError(429, 'too_many_attempts', 'x'))).toMatch(/too many attempts/i)
    expect(errorMessage(new ApiError(409, 'email_taken', 'email already registered'))).toMatch(/already exists/i)
    expect(errorMessage(new ApiError(500, 'internal', 'internal server error'))).toMatch(/server had a problem/i)
  })

  it('shows a working-hours time in the reader\'s terms instead of a raw timestamp', () => {
    const msg = errorMessage(
      new ApiError(409, 'outside_working_hours', "searches run during the account's working hours; next window opens 2026-11-09T09:30:00Z"),
    )
    expect(msg).toMatch(/next window opens/i)
    expect(msg).not.toContain('2026-11-09T09:30:00Z')
  })

  it('keeps the server wording when there is nothing better, and never leaks an unknown error object', () => {
    expect(errorMessage(new ApiError(409, 'no_leads', 'add at least one lead before starting'))).toBe('add at least one lead before starting')
    expect(errorMessage(new Error('boom'))).toBe('Something went wrong. Please try again.')
    expect(errorMessage('weird')).toBe('Something went wrong. Please try again.')
  })
})

describe('time formatting', () => {
  it('shows a moment in the account\'s timezone', () => {
    const s = formatMoment('2026-11-09T04:00:00Z', 'Asia/Kolkata')
    expect(s).toContain('09:30')
    expect(s).toMatch(/Mon/)
    expect(formatMoment('2026-11-09T04:00:00Z', 'UTC')).toContain('04:00')
  })
  it('survives a bad timezone or date', () => {
    expect(formatMoment('2026-11-09T04:00:00Z', 'Not/AZone')).not.toBe('')
    expect(formatMoment('garbage')).toBe('')
    expect(formatDate('garbage')).toBe('')
    expect(formatDate('2026-11-03T10:00:00Z')).toMatch(/2026/)
  })
})

describe('statusInfo', () => {
  it('labels every status the server can send', () => {
    const all: AccountStatus[] = ['pending', 'connected', 'frozen', 'reconnection_required', 'disconnected', 'error']
    for (const s of all) expect(statusInfo(s).label).not.toBe('')
    expect(statusInfo('connected').tone).toBe('good')
    expect(statusInfo('reconnection_required').tone).toBe('warn')
    expect(statusInfo('error').tone).toBe('bad')
  })
})

describe('timezones and weekdays', () => {
  it('always offers UTC and the timezone in use, sorted and without duplicates', () => {
    const list = timezones('Pacific/Chatham', 'Pacific/Chatham')
    expect(list).toContain('UTC')
    expect(list).toContain('Pacific/Chatham')
    expect(new Set(list).size).toBe(list.length)
    expect([...list].sort()).toEqual(list)
    expect(browserTimezone()).not.toBe('')
  })
  it('offers the names people look for, even where the browser lists an older alias', () => {
    expect(timezones()).toContain('Asia/Kolkata')
  })
  it('lists Monday first and uses the server\'s numbering (Sunday = 0)', () => {
    expect(WEEKDAYS.map((d) => d.value)).toEqual([1, 2, 3, 4, 5, 6, 0])
    expect(WEEKDAYS[6].short).toBe('Sun')
  })
})
