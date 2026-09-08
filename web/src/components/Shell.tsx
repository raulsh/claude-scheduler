import { useEffect, useState, type ReactNode } from 'react'
import { NavLink, Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Activity, CalendarClock, HeartPulse, Moon, Sun, Plus } from 'lucide-react'

import { api } from '../api/client'
import { Badge } from './ui'

type Theme = 'light' | 'dark' | 'system'

const THEME_KEY = 'claude-scheduler.theme'

function readTheme(): Theme {
  try {
    const stored = localStorage.getItem(THEME_KEY)
    if (stored === 'light' || stored === 'dark') return stored
  } catch {
    // Private windows and blocked site data both land here; the system
    // default is a fine answer.
  }
  return 'system'
}

export function Shell({ children }: { children: ReactNode }) {
  const [theme, setTheme] = useState<Theme>(readTheme)

  useEffect(() => {
    const root = document.documentElement
    if (theme === 'system') {
      root.removeAttribute('data-theme')
    } else {
      root.setAttribute('data-theme', theme)
    }
    try {
      if (theme === 'system') localStorage.removeItem(THEME_KEY)
      else localStorage.setItem(THEME_KEY, theme)
    } catch {
      // Persisting the preference is a convenience, not a requirement.
    }
  }, [theme])

  // The dependency banner is the one thing worth surfacing on every page.
  const { data: snapshot } = useQuery({
    queryKey: ['health', 'snapshot'],
    queryFn: () => api.healthSnapshot(false),
    refetchInterval: 60_000,
  })

  // Only dependencies an enabled task actually requires warrant a warning.
  // A machine can carry dozens of configured MCP connectors no schedule
  // references, and counting those would make this banner noise.
  const gating = snapshot?.blocking_declared ?? 0

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-20 border-b border-border-subtle bg-bg/85 backdrop-blur">
        <div className="mx-auto flex h-14 max-w-6xl items-center gap-1 px-4">
          <Link to="/" className="mr-4 flex items-center gap-2 font-semibold">
            <span className="grid h-6 w-6 place-items-center rounded bg-accent text-[13px] text-white">
              c
            </span>
            <span className="text-sm">Claude Scheduler</span>
          </Link>

          <Tab to="/" icon={<CalendarClock className="h-3.5 w-3.5" />}>
            Schedules
          </Tab>
          <Tab to="/executions" icon={<Activity className="h-3.5 w-3.5" />}>
            Executions
          </Tab>
          <Tab to="/health" icon={<HeartPulse className="h-3.5 w-3.5" />}>
            Dependencies
            {gating > 0 ? (
              <span className="ml-1 rounded-full bg-st-blocked px-1.5 text-[10px] font-semibold text-black">
                {gating}
              </span>
            ) : null}
          </Tab>

          <div className="ml-auto flex items-center gap-2">
            <Link
              to="/tasks/new"
              className="inline-flex items-center gap-1.5 rounded-md bg-accent px-2.5 py-1.5 text-xs font-medium text-white transition hover:opacity-90"
            >
              <Plus className="h-3.5 w-3.5" />
              New schedule
            </Link>
            <button
              onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}
              title="Toggle theme"
              aria-label="Toggle theme"
              className="rounded-md p-1.5 text-fg-muted transition hover:bg-surface-2 hover:text-fg"
            >
              {theme === 'dark' ? (
                <Sun className="h-4 w-4" />
              ) : (
                <Moon className="h-4 w-4" />
              )}
            </button>
          </div>
        </div>
      </header>

      {gating > 0 ? (
        <div className="border-b border-st-blocked/30 bg-st-blocked/10">
          <div className="mx-auto flex max-w-6xl items-center gap-2 px-4 py-2 text-xs">
            <Badge tone="warn">{gating} blocking</Badge>
            <span className="text-fg-muted">
              {gating === 1 ? 'A dependency a schedule needs' : 'Dependencies your schedules need'}{' '}
              {gating === 1 ? 'is' : 'are'} unhealthy, so those runs will be refused.
            </span>
            <Link to="/health" className="ml-auto font-medium text-accent hover:underline">
              Review
            </Link>
          </div>
        </div>
      ) : null}

      <main className="mx-auto max-w-6xl px-4 py-6">{children}</main>
    </div>
  )
}

function Tab({
  to,
  icon,
  children,
}: {
  to: string
  icon: ReactNode
  children: ReactNode
}) {
  return (
    <NavLink
      to={to}
      end={to === '/'}
      className={({ isActive }) =>
        `inline-flex items-center gap-1.5 rounded-md px-2.5 py-1.5 text-xs font-medium transition ${
          isActive
            ? 'bg-surface-2 text-fg'
            : 'text-fg-muted hover:bg-surface-2 hover:text-fg'
        }`
      }
    >
      {icon}
      {children}
    </NavLink>
  )
}
