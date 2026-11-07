import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { api } from '../api/endpoints'
import type { ScheduleSettings, ScheduleView } from '../api/types'
import { Notice } from '../components/ui'
import { errorMessage, formatMoment, timezones, WEEKDAYS } from '../lib/format'
import { MAX_CONNECT, MAX_MESSAGE, validate } from '../lib/schedule'

function settingsOf(v: ScheduleView): ScheduleSettings {
  return {
    timezone: v.timezone,
    work_days: v.work_days,
    start: v.start,
    end: v.end,
    holidays: v.holidays,
    daily_connect_cap: v.daily_connect_cap,
    daily_message_cap: v.daily_message_cap,
  }
}

function same(a: ScheduleSettings, b: ScheduleSettings): boolean {
  return JSON.stringify(a) === JSON.stringify(b)
}

/** <input type="time"> cannot show 24:00, so a window that runs to midnight shows 23:59. */
const toTimeInput = (v: string) => (v === '24:00' ? '23:59' : v)

export function ScheduleForm({ accountId }: { accountId: number }) {
  const q = useQuery({ queryKey: ['schedule', accountId], queryFn: () => api.accounts.schedule(accountId) })
  if (q.isPending) return <p aria-busy="true">Loading schedule…</p>
  if (q.isError) return <Notice tone="error">{errorMessage(q.error)}</Notice>
  return <Editor accountId={accountId} view={q.data} />
}

function Editor({ accountId, view }: { accountId: number; view: ScheduleView }) {
  const qc = useQueryClient()
  const [saved, setSaved] = useState(view)
  const [form, setForm] = useState<ScheduleSettings>(settingsOf(view))
  const [holiday, setHoliday] = useState('')
  const [justSaved, setJustSaved] = useState(false)

  const save = useMutation({
    mutationFn: (s: ScheduleSettings) => api.accounts.saveSchedule(accountId, s),
    onSuccess(v) {
      setSaved(v)
      setForm(settingsOf(v))
      setJustSaved(true)
      qc.setQueryData(['schedule', accountId], v)
    },
  })

  const errors = validate(form)
  const dirty = !same(form, settingsOf(saved))
  const set = <K extends keyof ScheduleSettings>(k: K, v: ScheduleSettings[K]) => {
    setJustSaved(false)
    setForm((f) => ({ ...f, [k]: v }))
  }

  function toggleDay(d: number) {
    const days = form.work_days.includes(d) ? form.work_days.filter((x) => x !== d) : [...form.work_days, d]
    set('work_days', days.sort((a, b) => a - b))
  }

  function addHoliday() {
    if (!holiday || form.holidays.includes(holiday)) return
    set('holidays', [...form.holidays, holiday].sort())
    setHoliday('')
  }

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (Object.keys(errors).length === 0) save.mutate(form)
  }

  return (
    <form className="schedule" onSubmit={onSubmit} noValidate>
      <p className="window-status" aria-live="polite">
        {saved.open_now ? (
          <>
            <span className="dot dot-good" /> Sending is open now.
          </>
        ) : (
          <>
            <span className="dot dot-idle" /> Next send window: <strong>{formatMoment(saved.next_send_window, saved.timezone)}</strong>
          </>
        )}
      </p>
      <p className="hint">
        Reachout only sends connection requests and messages on the days and hours below, in this account&apos;s timezone.
        Replies and acceptances are recorded at any time.
      </p>

      <div className="field">
        <label htmlFor={`tz-${accountId}`}>Timezone</label>
        <select id={`tz-${accountId}`} value={form.timezone} onChange={(e) => set('timezone', e.target.value)}>
          {timezones(form.timezone).map((z) => (
            <option key={z} value={z}>
              {z}
            </option>
          ))}
        </select>
      </div>

      <fieldset className="field">
        <legend>Working days</legend>
        <div className="days">
          {WEEKDAYS.map((d) => (
            <label key={d.value} className="check">
              <input
                type="checkbox"
                checked={form.work_days.includes(d.value)}
                onChange={() => toggleDay(d.value)}
                aria-label={d.long}
              />
              <span>{d.short}</span>
            </label>
          ))}
        </div>
        {errors.days && <p className="field-error">{errors.days}</p>}
      </fieldset>

      <div className="row">
        <div className="field">
          <label htmlFor={`start-${accountId}`}>Opens</label>
          <input id={`start-${accountId}`} type="time" value={form.start} onChange={(e) => set('start', e.target.value)} />
        </div>
        <div className="field">
          <label htmlFor={`end-${accountId}`}>Closes</label>
          <input
            id={`end-${accountId}`}
            type="time"
            value={toTimeInput(form.end)}
            onChange={(e) => set('end', e.target.value)}
          />
        </div>
      </div>
      {errors.window && <p className="field-error">{errors.window}</p>}

      <div className="row">
        <div className="field">
          <label htmlFor={`cc-${accountId}`}>Connection requests per day</label>
          <input
            id={`cc-${accountId}`}
            type="number"
            min={0}
            max={MAX_CONNECT}
            value={Number.isNaN(form.daily_connect_cap) ? '' : form.daily_connect_cap}
            onChange={(e) => set('daily_connect_cap', e.target.valueAsNumber)}
          />
          {errors.connect && <p className="field-error">{errors.connect}</p>}
        </div>
        <div className="field">
          <label htmlFor={`mc-${accountId}`}>Messages per day</label>
          <input
            id={`mc-${accountId}`}
            type="number"
            min={0}
            max={MAX_MESSAGE}
            value={Number.isNaN(form.daily_message_cap) ? '' : form.daily_message_cap}
            onChange={(e) => set('daily_message_cap', e.target.valueAsNumber)}
          />
          {errors.message && <p className="field-error">{errors.message}</p>}
        </div>
      </div>
      <p className="hint">
        New accounts warm up automatically: at most 5 connection requests a day in the first week, 10 in the second and 20
        in the third, whatever you set here.
      </p>

      <fieldset className="field">
        <legend>Holidays (no sending)</legend>
        <div className="row row-end">
          <div className="field">
            <label htmlFor={`hol-${accountId}`}>Add a date</label>
            <input id={`hol-${accountId}`} type="date" value={holiday} onChange={(e) => setHoliday(e.target.value)} />
          </div>
          <button type="button" onClick={addHoliday} disabled={!holiday}>
            Add holiday
          </button>
        </div>
        {form.holidays.length > 0 && (
          <ul className="chips" aria-label="Holidays">
            {form.holidays.map((h) => (
              <li key={h}>
                {h}
                <button
                  type="button"
                  className="chip-remove"
                  aria-label={`Remove ${h}`}
                  onClick={() =>
                    set(
                      'holidays',
                      form.holidays.filter((x) => x !== h),
                    )
                  }
                >
                  ×
                </button>
              </li>
            ))}
          </ul>
        )}
      </fieldset>

      {save.isError && <Notice tone="error">{errorMessage(save.error)}</Notice>}
      {justSaved && !dirty && <Notice tone="success">Schedule saved.</Notice>}
      <button type="submit" className="primary" disabled={!dirty || Object.keys(errors).length > 0 || save.isPending}>
        {save.isPending ? 'Saving…' : 'Save schedule'}
      </button>
    </form>
  )
}
