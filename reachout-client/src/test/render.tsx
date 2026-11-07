import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render } from '@testing-library/react'
import type { ReactElement } from 'react'
import { MemoryRouter } from 'react-router-dom'
import { App } from '../App'
import { AuthProvider } from '../auth/AuthProvider'

export function newQueryClient() {
  return new QueryClient({
    // gcTime is left at its real default: cached data outlives the page that showed it, which
    // is exactly what sign-out has to clean up.
    defaultOptions: { queries: { retry: false, staleTime: 0, refetchOnWindowFocus: false } },
  })
}

/** Renders the whole app at `path`, with the same providers main.tsx uses. */
export function renderApp(path: string) {
  const qc = newQueryClient()
  const utils = render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>
        <AuthProvider>
          <App />
        </AuthProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
  return { ...utils, qc }
}

/** Renders one component inside the providers, for tests of a single screen part. */
export function renderWith(ui: ReactElement, path = '/') {
  const qc = newQueryClient()
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>{ui}</MemoryRouter>
    </QueryClientProvider>,
  )
}
