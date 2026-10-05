import { Link, Outlet, createFileRoute } from '@tanstack/react-router'

export const Route = createFileRoute('/_app')({ component: Layout, ssr: false })

const pages = [
  { to: '/', label: 'Overview' },
  { to: '/nodes', label: 'Nodes' },
  { to: '/sessions', label: 'Sessions' },
  { to: '/events', label: 'Events' },
] as const

function Layout() {
  return (
    <div className="mx-auto max-w-6xl px-4 pb-12">
      <nav className="flex flex-wrap items-center gap-1 py-4">
        <span className="mr-4 text-lg font-bold tracking-tight">⚡ Embolt</span>
        {pages.map((p) => (
          <Link
            key={p.to}
            to={p.to}
            activeOptions={{ exact: p.to === '/' }}
            className="rounded px-3 py-1.5 text-sm text-zinc-600 hover:bg-zinc-200/60 dark:text-zinc-400 dark:hover:bg-zinc-800"
            activeProps={{ className: 'bg-zinc-200 font-medium text-zinc-900 dark:bg-zinc-800 dark:text-zinc-100' }}
          >
            {p.label}
          </Link>
        ))}
      </nav>
      <Outlet />
    </div>
  )
}
