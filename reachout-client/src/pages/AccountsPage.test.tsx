import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it } from 'vitest'
import { timing } from '../config'
import { account, apiError, mockApi, schedule, sequence, signedIn } from '../test/mockApi'
import { renderApp } from '../test/render'

const LINK = 'https://app.linkedapi.io/connection/sess-1'
const original = { ...timing }
afterEach(() => Object.assign(timing, original))

function open() {
  renderApp('/accounts')
  return userEvent.setup()
}

describe('the LinkedIn page', () => {
  it('invites a new user to connect an account', async () => {
    mockApi(signedIn())
    open()
    expect(await screen.findByText('No LinkedIn account connected yet')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Connect LinkedIn account' })).toBeInTheDocument()
  })

  it('lists accounts with their status and what to do about it', async () => {
    mockApi({
      ...signedIn(),
      'GET /api/accounts': {
        body: {
          accounts: [
            account({ id: 1, name: 'Ada Lovelace' }),
            account({ id: 2, name: 'Grace Hopper', status: 'reconnection_required' }),
            account({ id: 3, name: 'Alan Turing', status: 'frozen' }),
            account({ id: 4, name: '', status: 'error', status_detail: 'LinkedIn login expired' }),
            account({ id: 5, name: 'Edsger', status: 'disconnected' }),
          ],
        },
      },
    })
    open()
    const ada = await screen.findByRole('article', { name: 'Ada Lovelace' })
    expect(within(ada).getByText('Connected')).toBeInTheDocument()
    expect(within(ada).getByText('Founder at Analytical Engines')).toBeInTheDocument()
    expect(within(ada).getByRole('link', { name: 'View profile' })).toHaveAttribute('href', 'https://www.linkedin.com/in/ada')
    expect(within(ada).getByRole('link', { name: 'View profile' })).toHaveAttribute('rel', expect.stringContaining('noopener'))

    const grace = screen.getByRole('article', { name: 'Grace Hopper' })
    expect(within(grace).getByText('Needs reconnecting')).toBeInTheDocument()
    expect(within(grace).getByText(/nothing is being sent/i)).toBeInTheDocument()
    expect(within(grace).getByRole('button', { name: 'Reconnect' })).toBeInTheDocument()

    expect(within(screen.getByRole('article', { name: 'Alan Turing' })).getByText(/frozen/i, { selector: '.notice *, .notice' })).toBeInTheDocument()
    const failed = screen.getByRole('article', { name: 'LinkedIn account' })
    expect(within(failed).getByText(/LinkedIn login expired/)).toBeInTheDocument()
    // Accounts that cannot send have no schedule to edit.
    expect(within(failed).queryByRole('button', { name: /sending schedule/i })).not.toBeInTheDocument()
    expect(within(screen.getByRole('article', { name: 'Edsger' })).getByText(/removed in Linked API/i)).toBeInTheDocument()
  })

  it('shows a load failure instead of an empty page', async () => {
    mockApi({ ...signedIn(), 'GET /api/accounts': apiError(502, 'linkedin_unavailable', 'LinkedIn service is unavailable, try again shortly') })
    open()
    expect(await screen.findByRole('alert')).toHaveTextContent(/unavailable/i)
  })

  it('re-checks an account on request', async () => {
    const api = mockApi({
      ...signedIn(),
      'GET /api/accounts': { body: { accounts: [account()] } },
      'POST /api/accounts/:id/refresh': { body: { account: account() } },
    })
    const u = open()
    await u.click(await screen.findByRole('button', { name: 'Check status' }))
    await waitFor(() => expect(api.called('POST', '/api/accounts/1/refresh')).toHaveLength(1))
  })
})

describe('connecting an account', () => {
  const connected = account({ id: 7, name: 'New Person' })

  function setup(extra: Parameters<typeof mockApi>[0] = {}) {
    return mockApi({
      ...signedIn(),
      'POST /api/accounts/connect': { status: 201, body: { account: account({ id: 7, status: 'pending', name: '', connection_link: LINK }) } },
      'POST /api/accounts/:id/refresh': sequence({ body: { account: account({ id: 7, status: 'pending', name: '' }) } }, { body: { account: connected } }),
      ...extra,
    })
  }

  it('walks through the LinkedIn login and shows the connected account', async () => {
    const api = setup()
    const u = open()
    await u.click(await screen.findByRole('button', { name: 'Connect LinkedIn account' }))

    // The account timezone is chosen first and sent along.
    const tz = (await screen.findByLabelText('Account timezone')) as HTMLSelectElement
    await u.selectOptions(tz, 'Asia/Kolkata')
    api.on('GET /api/accounts', { body: { accounts: [connected] } }) // what the list shows after connecting
    await u.click(screen.getByRole('button', { name: 'Continue to LinkedIn' }))

    expect(await screen.findByText(/waiting for you to finish signing in/i)).toBeInTheDocument()
    expect(api.called('POST', '/api/accounts/connect')[0].body).toEqual({ timezone: 'Asia/Kolkata' })
    expect(window.open).toHaveBeenCalledWith(LINK, '_blank', 'noopener')
    // The login page is also offered as a link, in case the browser blocked the new tab.
    expect(screen.getByRole('link', { name: 'Open it again' })).toHaveAttribute('href', LINK)

    expect(await screen.findByText(/connected as new person/i)).toBeInTheDocument()
    expect(api.called('POST', '/api/accounts/7/refresh').length).toBeGreaterThanOrEqual(2) // polled until it finished
    expect(await screen.findByRole('article', { name: 'New Person' })).toBeInTheDocument() // the list picked it up

    // Once it has finished, the panel is still open but nothing keeps asking the server.
    const n = api.called('POST', '/api/accounts/7/refresh').length
    await new Promise((r) => setTimeout(r, 80))
    expect(api.called('POST', '/api/accounts/7/refresh').length).toBe(n)

    await u.click(screen.getByRole('button', { name: 'Done' }))
    expect(screen.queryByRole('region', { name: /connecting linkedin/i })).not.toBeInTheDocument()
  })

  it('explains that another connection is in progress', async () => {
    setup({ 'POST /api/accounts/connect': apiError(409, 'connection_busy', 'another connection is in progress, try again in a few minutes') })
    const u = open()
    await u.click(await screen.findByRole('button', { name: 'Connect LinkedIn account' }))
    await u.click(await screen.findByRole('button', { name: 'Continue to LinkedIn' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/another connection is in progress/i)
    // Nothing started, so the form is still there to try again.
    expect(screen.getByRole('button', { name: 'Continue to LinkedIn' })).toBeEnabled()
  })

  it('explains a missing seat', async () => {
    setup({ 'POST /api/accounts/connect': apiError(409, 'no_seats', 'no LinkedIn seat is available on the subscription') })
    const u = open()
    await u.click(await screen.findByRole('button', { name: 'Connect LinkedIn account' }))
    await u.click(await screen.findByRole('button', { name: 'Continue to LinkedIn' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/no linkedin seat/i)
  })

  it('reports a login that failed and offers to start again', async () => {
    setup({
      'POST /api/accounts/:id/refresh': { body: { account: account({ id: 7, status: 'error', status_detail: 'LinkedIn login expired' }) } },
    })
    const u = open()
    await u.click(await screen.findByRole('button', { name: 'Connect LinkedIn account' }))
    await u.click(await screen.findByRole('button', { name: 'Continue to LinkedIn' }))
    expect(await screen.findByText(/did not complete: LinkedIn login expired/i)).toBeInTheDocument()
    await u.click(screen.getByRole('button', { name: 'Start again' }))
    expect(await screen.findByRole('button', { name: 'Continue to LinkedIn' })).toBeInTheDocument()
  })

  it('stops and offers a retry when checking fails', async () => {
    setup({ 'POST /api/accounts/:id/refresh': apiError(502, 'linkedin_unavailable', 'LinkedIn service is unavailable, try again shortly') })
    const u = open()
    await u.click(await screen.findByRole('button', { name: 'Connect LinkedIn account' }))
    await u.click(await screen.findByRole('button', { name: 'Continue to LinkedIn' }))
    expect(await screen.findByText(/service is unavailable/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Start again' })).toBeInTheDocument()
  })

  it('gives up on an attempt that never finishes', async () => {
    timing.connectTimeoutMs = 60
    setup({ 'POST /api/accounts/:id/refresh': { body: { account: account({ id: 7, status: 'pending', name: '' }) } } })
    const u = open()
    await u.click(await screen.findByRole('button', { name: 'Connect LinkedIn account' }))
    await u.click(await screen.findByRole('button', { name: 'Continue to LinkedIn' }))
    expect(await screen.findByText(/took too long and has expired/i)).toBeInTheDocument()
    expect(screen.queryByText(/waiting for you to finish/i)).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Start again' })).toBeInTheDocument()
  })

  it('can be cancelled before anything starts', async () => {
    const api = setup()
    const u = open()
    await u.click(await screen.findByRole('button', { name: 'Connect LinkedIn account' }))
    await u.click(await screen.findByRole('button', { name: 'Cancel' }))
    expect(screen.queryByLabelText('Account timezone')).not.toBeInTheDocument()
    expect(api.called('POST', '/api/accounts/connect')).toHaveLength(0)
  })
})

describe('accounts that need attention', () => {
  it('reconnects: opens the login page and follows the account until it is back', async () => {
    const api = mockApi({
      ...signedIn(),
      'GET /api/accounts': { body: { accounts: [account({ id: 2, name: 'Grace Hopper', status: 'reconnection_required' })] } },
      'POST /api/accounts/:id/reconnect': { body: { reconnection_link: 'https://app.linkedapi.io/connection/re-1' } },
      'POST /api/accounts/:id/refresh': sequence(
        { body: { account: account({ id: 2, name: 'Grace Hopper', status: 'reconnection_required' }) } },
        { body: { account: account({ id: 2, name: 'Grace Hopper', status: 'connected' }) } },
      ),
    })
    const u = open()
    await u.click(await screen.findByRole('button', { name: 'Reconnect' }))
    api.on('GET /api/accounts', { body: { accounts: [account({ id: 2, name: 'Grace Hopper' })] } })

    expect(api.called('POST', '/api/accounts/2/reconnect')).toHaveLength(1)
    expect(window.open).toHaveBeenCalledWith('https://app.linkedapi.io/connection/re-1', '_blank', 'noopener')
    expect(await screen.findByRole('link', { name: 'Open the LinkedIn login page' })).toHaveAttribute('href', 'https://app.linkedapi.io/connection/re-1')
    // Once LinkedIn is back, the badge follows without a manual refresh.
    expect(await within(await screen.findByRole('article', { name: 'Grace Hopper' })).findByText('Connected')).toBeInTheDocument()
  })

  it('shows a reconnect failure', async () => {
    mockApi({
      ...signedIn(),
      'GET /api/accounts': { body: { accounts: [account({ status: 'reconnection_required' })] } },
      'POST /api/accounts/:id/reconnect': apiError(409, 'not_reconnectable', 'account does not need to be reconnected'),
    })
    const u = open()
    await u.click(await screen.findByRole('button', { name: 'Reconnect' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/does not need to be reconnected/i)
  })

  it('keeps following a connection left over from an earlier visit', async () => {
    const api = mockApi({
      ...signedIn(),
      'GET /api/accounts': { body: { accounts: [account({ id: 9, name: '', status: 'pending' })] } },
      'POST /api/accounts/:id/refresh': sequence(
        { body: { account: account({ id: 9, status: 'pending' }) } },
        { body: { account: account({ id: 9, status: 'error', status_detail: 'connection timed out' }) } },
      ),
    })
    open()
    expect(await screen.findByText(/a connection is in progress/i)).toBeInTheDocument()
    await waitFor(() => expect(api.called('POST', '/api/accounts/9/refresh').length).toBeGreaterThanOrEqual(2))
  })
})

describe('the sending schedule', () => {
  it('opens and closes the schedule of a usable account', async () => {
    mockApi({
      ...signedIn(),
      'GET /api/accounts': { body: { accounts: [account()] } },
      'GET /api/accounts/:id/schedule': { body: schedule() },
    })
    const u = open()
    const toggle = await screen.findByRole('button', { name: 'Sending schedule' })
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    await u.click(toggle)
    expect(await screen.findByLabelText('Timezone')).toHaveValue('Asia/Kolkata')
    expect(screen.getByRole('button', { name: 'Hide sending schedule' })).toHaveAttribute('aria-expanded', 'true')
    await u.click(screen.getByRole('button', { name: 'Hide sending schedule' }))
    expect(screen.queryByLabelText('Timezone')).not.toBeInTheDocument()
  })
})
