import { Link, Outlet, createFileRoute } from '@tanstack/react-router'
import { useStatus } from '#/api/client'

export const Route = createFileRoute('/_app')({ component: Layout, ssr: false })

const pages = [
  { to: '/', label: 'Overview' },
  { to: '/nodes', label: 'Nodes' },
  { to: '/sessions', label: 'Sessions' },
  { to: '/events', label: 'Events' },
] as const

function Layout() {
  const { data: status, live } = useStatus()

  return (
    <div className="min-h-dvh">
      <header className="sticky top-0 z-10 border-b border-zinc-200/80 bg-zinc-50/80 backdrop-blur dark:border-zinc-800 dark:bg-zinc-950/80">
        <div className="mx-auto flex h-14 max-w-6xl items-center gap-6 px-4 sm:px-6">
          <Link to="/" className="flex items-center gap-2 font-semibold tracking-tight">
            <svg viewBox="0 0 24 24" className="size-5 fill-amber-400" aria-hidden>
              <path d="M13 2 4 14h7l-1 8 9-12h-7l1-8Z" />
            </svg>
            Embolt
          </Link>
          <nav className="flex min-w-0 gap-1 overflow-x-auto p-1 [scrollbar-width:none]">
            {pages.map((p) => (
              <Link
                key={p.to}
                to={p.to}
                activeOptions={{ exact: p.to === '/' }}
                className="rounded-md px-3 py-1.5 text-sm whitespace-nowrap text-zinc-500 transition-colors hover:text-zinc-900 dark:hover:text-zinc-100"
                activeProps={{ className: 'bg-white font-medium !text-zinc-900 shadow-sm ring-1 ring-zinc-200 dark:bg-zinc-800 dark:!text-zinc-100 dark:ring-zinc-700' }}
              >
                {p.label}
              </Link>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-2 text-xs text-zinc-500" title={live ? 'Live' : 'Reconnecting'}>
            {status && <span className="hidden sm:inline">{status.version}</span>}
            <span className="relative flex size-2">
              {live && <span className="absolute inset-0 animate-ping rounded-full bg-emerald-400 opacity-60" />}
              <span className={`relative size-2 rounded-full ${live ? 'bg-emerald-500' : 'bg-zinc-400'}`} />
            </span>
          </div>
        </div>
      </header>
      <main className="mx-auto max-w-6xl px-4 py-6 sm:px-6 sm:py-8">
        <Outlet />
      </main>
    </div>
  )
}
