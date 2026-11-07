import { Link } from 'react-router-dom'

export function NotFound() {
  return (
    <main className="auth">
      <div className="auth-card">
        <h1>Page not found</h1>
        <p className="muted">There is nothing at this address.</p>
        <p>
          <Link to="/leads">Go to your leads</Link>
        </p>
      </div>
    </main>
  )
}
