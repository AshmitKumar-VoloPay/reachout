import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { apiError, mockApi, signedIn, user, leadsPage, lead } from '../test/mockApi'
import { renderApp } from '../test/render'

const anonymous = { 'GET /api/auth/me': apiError(401, 'unauthorized', 'sign in required') }

describe('signing in', () => {
  it('sends the trimmed email and password, then shows the app', async () => {
    const api = mockApi({
      ...anonymous,
      'POST /api/auth/login': { body: { user } },
      'GET /api/leads': { body: leadsPage([lead(1)]) },
    })
    renderApp('/login')
    const u = userEvent.setup()
    await u.type(await screen.findByLabelText('Email'), '  ada@example.com  ')
    await u.type(screen.getByLabelText('Password'), 'correct horse battery')
    await u.click(screen.getByRole('button', { name: 'Sign in' }))

    expect(await screen.findByRole('heading', { name: 'Leads' })).toBeInTheDocument()
    expect(api.called('POST', '/api/auth/login')[0].body).toEqual({ email: 'ada@example.com', password: 'correct horse battery' })
    expect(screen.getByText('ada@example.com')).toBeInTheDocument() // the signed-in user in the top bar
  })

  it('says so when the password is wrong and lets the person try again', async () => {
    const api = mockApi({ ...anonymous, 'POST /api/auth/login': apiError(401, 'invalid_credentials', 'invalid email or password') })
    renderApp('/login')
    const u = userEvent.setup()
    await u.type(await screen.findByLabelText('Email'), 'ada@example.com')
    await u.type(screen.getByLabelText('Password'), 'wrong wrong wrong')
    await u.click(screen.getByRole('button', { name: 'Sign in' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('Email or password is incorrect.')
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeEnabled()
    expect(api.called('POST', '/api/auth/login')).toHaveLength(1)
  })

  it('does not offer to submit an empty form', async () => {
    mockApi(anonymous)
    renderApp('/login')
    expect(await screen.findByRole('button', { name: 'Sign in' })).toBeDisabled()
  })

  it('shows a lock-out message after too many attempts', async () => {
    mockApi({ ...anonymous, 'POST /api/auth/login': apiError(429, 'too_many_attempts', 'too many attempts, try again later') })
    renderApp('/login')
    const u = userEvent.setup()
    await u.type(await screen.findByLabelText('Email'), 'ada@example.com')
    await u.type(screen.getByLabelText('Password'), 'whatever password')
    await u.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/too many attempts/i)
  })

  it('reports an unreachable server', async () => {
    mockApi(anonymous)
    const api = mockApi({ ...anonymous })
    api.fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
      if (String(input).includes('/auth/me')) return new Response(JSON.stringify({ error: { code: 'unauthorized', message: '' } }), { status: 401 })
      throw new TypeError('fetch failed')
    })
    renderApp('/login')
    const u = userEvent.setup()
    await u.type(await screen.findByLabelText('Email'), 'ada@example.com')
    await u.type(screen.getByLabelText('Password'), 'whatever password')
    await u.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/cannot reach the server/i)
  })
})

describe('signing up', () => {
  it('requires a long enough password before asking the server', async () => {
    const api = mockApi(anonymous)
    renderApp('/signup')
    const u = userEvent.setup()
    await u.type(await screen.findByLabelText('Email'), 'new@example.com')
    await u.type(screen.getByLabelText('Password'), 'short')
    await u.click(screen.getByRole('button', { name: 'Create account' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/at least 10 characters/i)
    expect(api.called('POST', '/api/auth/signup')).toHaveLength(0)
  })

  it('creates the account and starts on the LinkedIn page', async () => {
    const api = mockApi({
      ...anonymous,
      'POST /api/auth/signup': { status: 201, body: { user } },
      'GET /api/accounts': { body: { accounts: [] } },
    })
    renderApp('/signup')
    const u = userEvent.setup()
    await u.type(await screen.findByLabelText('Email'), 'new@example.com')
    await u.type(screen.getByLabelText('Password'), 'a long enough password')
    await u.click(screen.getByRole('button', { name: 'Create account' }))

    expect(await screen.findByRole('heading', { name: 'LinkedIn accounts' })).toBeInTheDocument()
    expect(api.called('POST', '/api/auth/signup')[0].body).toEqual({ email: 'new@example.com', password: 'a long enough password' })
  })

  it('explains that the email is taken', async () => {
    mockApi({ ...anonymous, 'POST /api/auth/signup': apiError(409, 'email_taken', 'email already registered') })
    renderApp('/signup')
    const u = userEvent.setup()
    await u.type(await screen.findByLabelText('Email'), 'ada@example.com')
    await u.type(screen.getByLabelText('Password'), 'a long enough password')
    await u.click(screen.getByRole('button', { name: 'Create account' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/already exists/i)
  })

  it('links between the two forms', async () => {
    mockApi(anonymous)
    renderApp('/login')
    await userEvent.setup().click(await screen.findByRole('link', { name: 'Create an account' }))
    expect(await screen.findByRole('button', { name: 'Create account' })).toBeInTheDocument()
  })
})

describe('the route guard', () => {
  it('sends someone who is not signed in to the login page', async () => {
    mockApi(anonymous)
    renderApp('/leads')
    expect(await screen.findByRole('button', { name: 'Sign in' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Leads' })).not.toBeInTheDocument()
  })

  it('returns to the page they asked for after signing in', async () => {
    mockApi({ ...anonymous, 'POST /api/auth/login': { body: { user } }, 'GET /api/accounts': { body: { accounts: [] } } })
    renderApp('/accounts')
    const u = userEvent.setup()
    await u.type(await screen.findByLabelText('Email'), 'ada@example.com')
    await u.type(screen.getByLabelText('Password'), 'correct horse battery')
    await u.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByRole('heading', { name: 'LinkedIn accounts' })).toBeInTheDocument()
  })

  it('keeps a signed-in person out of the login page', async () => {
    mockApi(signedIn())
    renderApp('/login')
    expect(await screen.findByRole('heading', { name: 'Leads' })).toBeInTheDocument()
  })

  it('sends the root address to the leads page', async () => {
    mockApi(signedIn())
    renderApp('/')
    expect(await screen.findByRole('heading', { name: 'Leads' })).toBeInTheDocument()
  })

  it('returns to the login page when the session ends mid-use', async () => {
    const api = mockApi(signedIn())
    renderApp('/leads')
    expect(await screen.findByRole('heading', { name: 'Leads' })).toBeInTheDocument()
    // The next request finds the session gone.
    api.on('GET /api/accounts', apiError(401, 'unauthorized', 'sign in required'))
    await userEvent.setup().click(screen.getByRole('link', { name: 'LinkedIn' }))
    expect(await screen.findByRole('button', { name: 'Sign in' })).toBeInTheDocument()
  })

  it('signs out and forgets the user', async () => {
    const api = mockApi({ ...signedIn(), 'POST /api/auth/logout': { status: 204 } })
    renderApp('/leads')
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Sign out' }))
    expect(await screen.findByRole('button', { name: 'Sign in' })).toBeInTheDocument()
    expect(api.called('POST', '/api/auth/logout')).toHaveLength(1)
    // Back to a signed-out state even if the server answers "me" with 401 now.
    api.on('GET /api/auth/me', apiError(401, 'unauthorized', 'x'))
    await waitFor(() => expect(screen.queryByText('ada@example.com')).not.toBeInTheDocument())
  })

  it('does not show the previous person\'s data to the next person on the same browser', async () => {
    // A fake server that behaves like the real one: it knows who is signed in, and refuses
    // protected requests from nobody.
    let session: 'ada' | 'bob' | null = 'ada'
    let release: () => void = () => {}
    const bobsLeadsGate = new Promise<void>((r) => (release = r))
    const unauthorized = apiError(401, 'unauthorized', 'sign in required')
    mockApi({
      'GET /api/auth/me': () => (session ? { body: { user: { ...user, id: session === 'ada' ? 1 : 2, email: `${session}@example.com` } } } : unauthorized),
      'POST /api/auth/logout': () => {
        session = null
        return { status: 204 }
      },
      'POST /api/auth/login': () => {
        session = 'bob'
        return { body: { user: { ...user, id: 2, email: 'bob@example.com' } } }
      },
      'GET /api/accounts': () => (session ? { body: { accounts: [] } } : unauthorized),
      'GET /api/leads': async () => {
        if (!session) return unauthorized
        if (session === 'ada') return { body: leadsPage([lead(1, { name: 'Private Person' })]) }
        await bobsLeadsGate // Bob's leads take a moment to arrive
        return { body: leadsPage([]) }
      },
    })
    renderApp('/leads')
    const u = userEvent.setup()
    expect(await screen.findByText('Private Person')).toBeInTheDocument()
    await u.click(screen.getByRole('button', { name: 'Sign out' }))

    await u.type(await screen.findByLabelText('Email'), 'bob@example.com')
    await u.type(screen.getByLabelText('Password'), 'correct horse battery')
    await u.click(screen.getByRole('button', { name: 'Sign in' }))

    // Until Bob's data arrives, nothing of Ada's may be on screen: a cache that survived
    // sign-out would show it for exactly this moment.
    expect(await screen.findByText('Loading leads…')).toBeInTheDocument()
    expect(screen.queryByText('Private Person')).not.toBeInTheDocument()
    release()
    expect(await screen.findByText('No leads yet')).toBeInTheDocument()
  })

  it('does not pretend to sign out when the server could not', async () => {
    mockApi({ ...signedIn(), 'POST /api/auth/logout': apiError(500, 'internal', 'boom') })
    renderApp('/leads')
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Sign out' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/could not sign out/i)
    // Still signed in, still on the page.
    expect(screen.getByText('ada@example.com')).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Leads' })).toBeInTheDocument()
  })

  it('treats "already signed out" as a successful sign-out', async () => {
    mockApi({ ...signedIn(), 'POST /api/auth/logout': apiError(401, 'unauthorized', 'sign in required') })
    renderApp('/leads')
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Sign out' }))
    expect(await screen.findByRole('button', { name: 'Sign in' })).toBeInTheDocument()
  })

  it('shows a page for unknown addresses', async () => {
    mockApi(signedIn())
    renderApp('/nope')
    expect(await screen.findByRole('heading', { name: 'Page not found' })).toBeInTheDocument()
  })
})
