import { fireEvent, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { apiError, mockApi, schedule } from '../test/mockApi'
import { renderWith } from '../test/render'
import { ScheduleForm } from './ScheduleForm'

function setup(view = schedule(), extra: Parameters<typeof mockApi>[0] = {}) {
  const api = mockApi({
    'GET /api/accounts/:id/schedule': { body: view },
    'PUT /api/accounts/:id/schedule': (req) => ({ body: { ...view, ...req.body, open_now: true } }),
    ...extra,
  })
  renderWith(<ScheduleForm accountId={1} />)
  return { api, u: userEvent.setup() }
}

const save = () => screen.getByRole('button', { name: /save schedule|saving/i })
const connectCap = () => screen.getByLabelText('Connection requests per day')

describe('what the schedule says', () => {
  it('shows the next send window in the account\'s timezone when sending is closed', async () => {
    setup(schedule({ open_now: false, next_send_window: '2026-11-09T04:00:00Z', timezone: 'Asia/Kolkata' }))
    const status = await screen.findByText(/next send window/i)
    expect(status).toHaveTextContent('09:30') // Monday 09:30 in Kolkata, not the browser's own time
    expect(status).toHaveTextContent('Mon')
  })

  it('says when sending is open right now', async () => {
    setup(schedule({ open_now: true }))
    expect(await screen.findByText('Sending is open now.')).toBeInTheDocument()
  })

  it('loads the stored values', async () => {
    setup(schedule({ work_days: [1, 3], start: '08:00', end: '16:30', holidays: ['2026-12-25'], daily_connect_cap: 7, daily_message_cap: 12 }))
    expect(await screen.findByLabelText('Timezone')).toHaveValue('Asia/Kolkata')
    expect(screen.getByLabelText('Monday')).toBeChecked()
    expect(screen.getByLabelText('Tuesday')).not.toBeChecked()
    expect(screen.getByLabelText('Wednesday')).toBeChecked()
    expect(screen.getByLabelText('Sunday')).not.toBeChecked()
    expect(screen.getByLabelText('Opens')).toHaveValue('08:00')
    expect(screen.getByLabelText('Closes')).toHaveValue('16:30')
    expect(connectCap()).toHaveValue(7)
    expect(screen.getByLabelText('Messages per day')).toHaveValue(12)
    expect(screen.getByRole('list', { name: 'Holidays' })).toHaveTextContent('2026-12-25')
  })

  it('explains the warm-up limits', async () => {
    setup()
    expect(await screen.findByText(/at most 5 connection requests a day in the first week/i)).toBeInTheDocument()
  })

  it('shows why the schedule could not be loaded', async () => {
    setup(schedule(), { 'GET /api/accounts/:id/schedule': apiError(404, 'not_found', 'account not found') })
    expect(await screen.findByRole('alert')).toHaveTextContent('account not found')
  })
})

describe('saving', () => {
  it('starts with nothing to save, and saves exactly what was changed', async () => {
    const { api, u } = setup()
    await screen.findByLabelText('Timezone')
    expect(save()).toBeDisabled()

    await u.clear(connectCap())
    await u.type(connectCap(), '8')
    await u.click(screen.getByLabelText('Saturday'))
    await u.click(screen.getByLabelText('Monday')) // off
    await u.click(screen.getByLabelText('Monday')) // and on again: no net change to the days order
    expect(save()).toBeEnabled()
    await u.click(save())

    expect(await screen.findByText('Schedule saved.')).toBeInTheDocument()
    expect(api.called('PUT', '/api/accounts/1/schedule')[0].body).toEqual({
      timezone: 'Asia/Kolkata',
      work_days: [1, 2, 3, 4, 5, 6], // sorted, whatever order they were clicked in
      start: '09:30',
      end: '17:30',
      holidays: [],
      daily_connect_cap: 8,
      daily_message_cap: 10,
    })
    // The server answered that sending is open, and the form now matches what was saved.
    expect(await screen.findByText('Sending is open now.')).toBeInTheDocument()
    expect(save()).toBeDisabled()
  })

  it('changes the timezone and the hours', async () => {
    const { api, u } = setup()
    await u.selectOptions(await screen.findByLabelText('Timezone'), 'Europe/London')
    fireEvent.change(screen.getByLabelText('Opens'), { target: { value: '10:00' } })
    fireEvent.change(screen.getByLabelText('Closes'), { target: { value: '16:00' } })
    await u.click(save())
    await screen.findByText('Schedule saved.')
    expect(api.called('PUT', '/api/accounts/1/schedule')[0].body).toMatchObject({ timezone: 'Europe/London', start: '10:00', end: '16:00' })
  })

  it('shows what the server said when it refuses', async () => {
    const { u } = setup(schedule(), { 'PUT /api/accounts/:id/schedule': apiError(400, 'invalid_schedule', 'invalid schedule: unknown timezone "Mars/Base"') })
    await u.clear(await screen.findByLabelText('Messages per day'))
    await u.type(screen.getByLabelText('Messages per day'), '20')
    await u.click(save())
    expect(await screen.findByRole('alert')).toHaveTextContent(/unknown timezone/i)
    expect(save()).toBeEnabled() // the change is still there to fix and retry
    expect(screen.queryByText('Schedule saved.')).not.toBeInTheDocument()
  })

  it('does not offer to save while a save is running', async () => {
    let release: () => void = () => {}
    const gate = new Promise<void>((r) => (release = r))
    const { u } = setup(schedule(), {
      'PUT /api/accounts/:id/schedule': async (req) => {
        await gate
        return { body: { ...schedule(), ...req.body } }
      },
    })
    await u.clear(await screen.findByLabelText('Messages per day'))
    await u.type(screen.getByLabelText('Messages per day'), '20')
    await u.click(save())
    expect(await screen.findByRole('button', { name: 'Saving…' })).toBeDisabled()
    release()
    expect(await screen.findByText('Schedule saved.')).toBeInTheDocument()
  })

  it('clears the "saved" message as soon as something changes again', async () => {
    const { u } = setup()
    await u.clear(await screen.findByLabelText('Messages per day'))
    await u.type(screen.getByLabelText('Messages per day'), '20')
    await u.click(save())
    await screen.findByText('Schedule saved.')
    await u.type(screen.getByLabelText('Messages per day'), '5')
    expect(screen.queryByText('Schedule saved.')).not.toBeInTheDocument()
  })

  it('keeps a window that runs to midnight when only something else changes', async () => {
    const { api, u } = setup(schedule({ end: '24:00' }))
    // A time field cannot show 24:00, so it shows the last minute of the day.
    expect(await screen.findByLabelText('Closes')).toHaveValue('23:59')
    expect(save()).toBeDisabled() // looking at it is not a change
    await u.clear(connectCap())
    await u.type(connectCap(), '6')
    await u.click(save())
    await screen.findByText('Schedule saved.')
    expect(api.called('PUT', '/api/accounts/1/schedule')[0].body.end).toBe('24:00')
  })
})

describe('checking before saving', () => {
  it('needs at least one working day', async () => {
    const { u } = setup(schedule({ work_days: [1] }))
    await u.click(await screen.findByLabelText('Monday'))
    expect(screen.getByText('Pick at least one working day.')).toBeInTheDocument()
    expect(save()).toBeDisabled()
    await u.click(screen.getByLabelText('Tuesday'))
    expect(screen.queryByText('Pick at least one working day.')).not.toBeInTheDocument()
    expect(save()).toBeEnabled()
  })

  it('needs the window to end after it starts', async () => {
    setup()
    fireEvent.change(await screen.findByLabelText('Closes'), { target: { value: '09:00' } })
    expect(screen.getByText('The window must end after it starts.')).toBeInTheDocument()
    expect(save()).toBeDisabled()
  })

  it('keeps the daily limits within what is allowed', async () => {
    const { u } = setup()
    await u.clear(await screen.findByLabelText('Connection requests per day'))
    await u.type(connectCap(), '31')
    expect(screen.getByText(/whole number from 0 to 30/i)).toBeInTheDocument()
    expect(save()).toBeDisabled()
    await u.clear(connectCap())
    expect(screen.getByText(/whole number from 0 to 30/i)).toBeInTheDocument() // empty is not a number
    await u.type(connectCap(), '30')
    expect(screen.queryByText(/whole number from 0 to 30/i)).not.toBeInTheDocument()

    await u.clear(screen.getByLabelText('Messages per day'))
    await u.type(screen.getByLabelText('Messages per day'), '101')
    expect(screen.getByText(/whole number from 0 to 100/i)).toBeInTheDocument()
  })
})

describe('holidays', () => {
  it('adds dates in order, ignores repeats, and removes them', async () => {
    const { api, u } = setup()
    const add = async (d: string) => {
      fireEvent.change(await screen.findByLabelText('Add a date'), { target: { value: d } })
      await u.click(screen.getByRole('button', { name: 'Add holiday' }))
    }
    expect(await screen.findByRole('button', { name: 'Add holiday' })).toBeDisabled()
    await add('2026-12-25')
    await add('2026-12-24')
    await add('2026-12-25') // a repeat
    const list = screen.getByRole('list', { name: 'Holidays' })
    expect(Array.from(list.querySelectorAll('li')).map((li) => li.textContent?.replace('×', ''))).toEqual(['2026-12-24', '2026-12-25'])

    await u.click(screen.getByRole('button', { name: 'Remove 2026-12-24' }))
    await u.click(save())
    await screen.findByText('Schedule saved.')
    expect(api.called('PUT', '/api/accounts/1/schedule')[0].body.holidays).toEqual(['2026-12-25'])
  })
})
