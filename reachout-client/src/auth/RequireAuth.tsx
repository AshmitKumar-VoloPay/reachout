import type { ReactNode } from 'react'
import { Navigate, useLocation } from 'react-router-dom'
import { useAuth } from './context'

/** Shows its children only to a signed-in user; everyone else goes to the login page. */
export function RequireAuth({ children }: { children: ReactNode }) {
  const { user, loading } = useAuth()
  const location = useLocation()
  if (loading) return <p className="page-loading" aria-busy="true">Loading…</p>
  if (!user) return <Navigate to="/login" replace state={{ from: location.pathname + location.search }} />
  return children
}

/** Login and signup pages: a signed-in user is sent on to the app instead. */
export function PublicOnly({ children }: { children: ReactNode }) {
  const { user, loading } = useAuth()
  if (loading) return <p className="page-loading" aria-busy="true">Loading…</p>
  if (user) return <Navigate to="/leads" replace />
  return children
}
