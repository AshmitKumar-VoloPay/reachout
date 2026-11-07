import { useState, type FormEvent } from 'react'
import { Link, Navigate, useLocation } from 'react-router-dom'
import { useAuth } from '../auth/context'
import { errorMessage } from '../lib/format'
import { Notice } from '../components/ui'

const MIN_PASSWORD = 10

export function AuthPage({ mode }: { mode: 'login' | 'signup' }) {
  const { user, login, signup } = useAuth()
  const location = useLocation()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  // Where to go once signed in. A new account starts by connecting LinkedIn.
  const [target, setTarget] = useState('/leads')

  if (user) return <Navigate to={target} replace />

  const isSignup = mode === 'signup'

  async function onSubmit(e: FormEvent) {
    e.preventDefault()
    setError('')
    if (isSignup && password.length < MIN_PASSWORD) {
      setError(`Use at least ${MIN_PASSWORD} characters for the password.`)
      return
    }
    const from = (location.state as { from?: string } | null)?.from
    setTarget(isSignup ? '/accounts' : from && from !== '/login' ? from : '/leads')
    setBusy(true)
    try {
      await (isSignup ? signup : login)(email.trim(), password)
    } catch (err) {
      setError(errorMessage(err))
      setBusy(false)
    }
  }

  return (
    <main className="auth">
      <div className="auth-card">
        <h1>Reachout</h1>
        <p className="muted">{isSignup ? 'Create your account' : 'Sign in to your account'}</p>
        <form onSubmit={onSubmit} noValidate>
          <div className="field">
            <label htmlFor="email">Email</label>
            <input
              id="email"
              type="email"
              autoComplete="email"
              required
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
          </div>
          <div className="field">
            <label htmlFor="password">Password</label>
            <input
              id="password"
              type="password"
              autoComplete={isSignup ? 'new-password' : 'current-password'}
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              aria-describedby={isSignup ? 'password-hint' : undefined}
            />
            {isSignup && (
              <p id="password-hint" className="hint">
                At least {MIN_PASSWORD} characters.
              </p>
            )}
          </div>
          {error && <Notice tone="error">{error}</Notice>}
          <button type="submit" className="primary" disabled={busy || !email || !password}>
            {busy ? 'Please wait…' : isSignup ? 'Create account' : 'Sign in'}
          </button>
        </form>
        <p className="auth-switch">
          {isSignup ? (
            <>
              Already have an account? <Link to="/login">Sign in</Link>
            </>
          ) : (
            <>
              New here? <Link to="/signup">Create an account</Link>
            </>
          )}
        </p>
      </div>
    </main>
  )
}
