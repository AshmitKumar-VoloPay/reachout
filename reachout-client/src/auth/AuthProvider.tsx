import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo, type ReactNode } from 'react'
import { api } from '../api/endpoints'
import { ApiError, configureApi } from '../api/client'
import { AuthContext, type AuthState } from './context'

const ME = ['me'] as const

export function AuthProvider({ children }: { children: ReactNode }) {
  const qc = useQueryClient()
  const me = useQuery({ queryKey: ME, queryFn: api.auth.me, retry: false, staleTime: Infinity })

  // A request that comes back 401 means the session ended: drop the user, and the route
  // guard sends them to the login page.
  useEffect(() => {
    configureApi({ onUnauthorized: () => qc.setQueryData(ME, null) })
    return () => configureApi({ onUnauthorized: undefined })
  }, [qc])

  const value = useMemo<AuthState>(
    () => ({
      user: me.data ?? null,
      loading: me.isPending,
      async login(email, password) {
        qc.setQueryData(ME, await api.auth.login(email, password))
      },
      async signup(email, password) {
        qc.setQueryData(ME, await api.auth.signup(email, password))
      },
      async logout() {
        try {
          await api.auth.logout()
        } catch (err) {
          // A 401 means the session was already gone. Anything else means the server still
          // considers this person signed in, so do not pretend otherwise.
          if (!(err instanceof ApiError && err.status === 401)) throw err
        }
        // Forget everything that belonged to this user. The "who am I" query itself stays,
        // because the mounted provider is watching it (clearing the whole cache would detach it).
        qc.removeQueries({ predicate: (q) => q.queryKey[0] !== ME[0] })
        qc.setQueryData(ME, null)
      },
    }),
    [me.data, me.isPending, qc],
  )
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}
