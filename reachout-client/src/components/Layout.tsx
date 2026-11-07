import { useState } from 'react'
import { NavLink, Outlet, useNavigate } from 'react-router-dom'
import { useAuth } from '../auth/context'
import { errorMessage } from '../lib/format'
import { Notice } from './ui'

export function Layout() {
  const { user, logout } = useAuth()
  const navigate = useNavigate()

  const [error, setError] = useState('')

  async function signOut() {
    setError('')
    try {
      await logout()
      navigate('/login', { replace: true })
    } catch (err) {
      setError(`Could not sign out. ${errorMessage(err)}`)
    }
  }

  return (
    <>
      <header className="topbar">
        <div className="topbar-inner">
          <span className="brand">Reachout</span>
          <nav aria-label="Main">
            <NavLink to="/leads">Leads</NavLink>
            <NavLink to="/accounts">LinkedIn</NavLink>
          </nav>
          <div className="topbar-user">
            <span className="muted" title={user?.email}>
              {user?.email}
            </span>
            <button type="button" onClick={signOut}>
              Sign out
            </button>
          </div>
        </div>
      </header>
      <div className="content">
        {error && <Notice tone="error">{error}</Notice>}
        <Outlet />
      </div>
    </>
  )
}
