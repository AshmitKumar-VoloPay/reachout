import { describe, expect, it } from 'vitest'
import type { ScheduleSettings } from '../api/types'
import { MAX_CONNECT, MAX_MESSAGE, validate } from './schedule'

const ok: ScheduleSettings = {
  timezone: 'UTC', work_days: [1, 2, 3, 4, 5], start: '09:30', end: '17:30', holidays: [],
  daily_connect_cap: 5, daily_message_cap: 10,
}

describe('validate', () => {
  it('accepts sensible settings', () => {
    expect(validate(ok)).toEqual({})
    expect(validate({ ...ok, daily_connect_cap: 0, daily_message_cap: 0 })).toEqual({})
    expect(validate({ ...ok, daily_connect_cap: MAX_CONNECT, daily_message_cap: MAX_MESSAGE })).toEqual({})
    expect(validate({ ...ok, end: '24:00' })).toEqual({})
  })
  it('needs at least one working day', () => {
    expect(validate({ ...ok, work_days: [] }).days).toBeTruthy()
  })
  it('needs a window that ends after it starts', () => {
    expect(validate({ ...ok, start: '17:30', end: '09:30' }).window).toBeTruthy()
    expect(validate({ ...ok, start: '09:00', end: '09:00' }).window).toBeTruthy()
    expect(validate({ ...ok, start: '', end: '17:00' }).window).toBeTruthy()
  })
  it('keeps the daily caps within what the server allows', () => {
    expect(validate({ ...ok, daily_connect_cap: MAX_CONNECT + 1 }).connect).toBeTruthy()
    expect(validate({ ...ok, daily_connect_cap: -1 }).connect).toBeTruthy()
    expect(validate({ ...ok, daily_connect_cap: 2.5 }).connect).toBeTruthy()
    expect(validate({ ...ok, daily_connect_cap: NaN }).connect).toBeTruthy()
    expect(validate({ ...ok, daily_message_cap: MAX_MESSAGE + 1 }).message).toBeTruthy()
    expect(validate({ ...ok, daily_message_cap: NaN }).message).toBeTruthy()
  })
})
