import { Link } from 'react-router-dom'
import { EmptyState } from '../components/ui'

export function NotFound() {
  return (
    <EmptyState
      title="Page not found"
      body="That URL does not match anything in the scheduler."
      action={
        <Link
          to="/"
          className="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-white hover:opacity-90"
        >
          Back to schedules
        </Link>
      }
    />
  )
}
