import type { ReactNode } from 'react'
import type { AccountStatus } from '../api/types'
import { statusInfo } from '../lib/format'

/** An inline message. Errors are announced to screen readers; other tones are polite. */
export function Notice({
  tone = 'info',
  children,
}: {
  tone?: 'info' | 'error' | 'success' | 'warn'
  children: ReactNode
}) {
  return (
    <div className={`notice notice-${tone}`} role={tone === 'error' ? 'alert' : 'status'}>
      {children}
    </div>
  )
}

export function StatusBadge({ status }: { status: AccountStatus }) {
  const { label, tone } = statusInfo(status)
  return <span className={`badge badge-${tone}`}>{label}</span>
}

export function Empty({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="empty">
      <h3>{title}</h3>
      {children && <p>{children}</p>}
    </div>
  )
}
