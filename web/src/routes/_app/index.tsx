import { Link, createFileRoute } from '@tanstack/react-router'
import { useNodes, useSession, useStatus } from '#/api/client'
import type { Limits, Session } from '#/api/types.gen'
import { SessionChart } from '#/components/chart'
import { SessionTable } from '#/components/sessions'
import { Badge, Empty, NodeLink, Panel, SessionName, State, Stat, Stats, clock } from '#/components/ui'

export const Route = createFileRoute('/_app/')({ component: Overview })

function Overview() {
  const { data: status } = useStatus()
  const { data: nodes } = useNodes()
  if (!status) return null
  const open = (nodes ?? []).filter((n) => n.breakerOpen)

  return (
    <div className="space-y-6">
      <Stats>
        <Stat label="Upstream">{status.upstream}</Stat>
        <Stat label="Primary">{status.primary ? <NodeLink {...status.primary} /> : '—'}</Stat>
        <Stat label="Nodes" sub={`${status.measured} measured`}>
          {status.usable}
          <span className="text-zinc-400"> / {status.nodes}</span>
        </Stat>
        <Stat label="Speed test">
          {status.testing ? (
            <NodeLink {...status.testing} />
          ) : status.testsPaused ? (
            <span className="text-amber-600 dark:text-amber-400">Paused to {clock(status.testsPaused)}</span>
          ) : (
            <span className="text-zinc-400">—</span>
          )}
        </Stat>
      </Stats>

      <section className="space-y-3">
        <h2 className="text-sm font-semibold">Playing</h2>
        {status.sessions.length === 0 ? (
          <div className="rounded-xl bg-white ring-1 ring-zinc-200/80 dark:bg-zinc-900 dark:ring-zinc-800">
            <Empty>Nothing playing</Empty>
          </div>
        ) : (
          <div className="grid gap-3 lg:grid-cols-2">
            {status.sessions.map((s) => (
              <PlayingCard key={s.key} s={s} limits={status.limits} />
            ))}
          </div>
        )}
      </section>

      {open.length > 0 && (
        <Panel title="Open breakers" aside={<Badge tone="bad">{open.length}</Badge>} flush>
          <ul className="divide-y divide-zinc-100 border-t border-zinc-100 text-sm dark:divide-zinc-800 dark:border-zinc-800">
            {open.map((n) => (
              <li key={n.id} className="flex justify-between gap-4 px-5 py-3">
                <NodeLink id={n.id} name={n.name} className="truncate" />
                <span className="shrink-0 tabular-nums text-zinc-500">to {clock(n.openUntil!)}</span>
              </li>
            ))}
          </ul>
        </Panel>
      )}

      {status.recent.length > 0 && (
        <Panel title="Last 24 hours" flush>
          <SessionTable sessions={status.recent} limits={status.limits} />
        </Panel>
      )}
    </div>
  )
}

function PlayingCard({ s, limits }: { s: Session; limits: Limits }) {
  const { data: d } = useSession(s.key)
  return (
    <Link
      to="/sessions/$key"
      params={{ key: s.key }}
      className="block rounded-xl bg-white p-4 ring-1 ring-zinc-200/80 transition-colors hover:ring-zinc-300 dark:bg-zinc-900 dark:ring-zinc-800 dark:hover:ring-zinc-700"
    >
      <div className="flex items-start justify-between gap-3">
        <SessionName s={s} link={false} />
        <State state={s.state} />
      </div>
      <div className="mt-4">
        <SessionChart history={d?.history ?? []} events={d?.events} limits={limits} bitrate={s.bitrateMbps} compact />
      </div>
      <dl className="mt-3 grid grid-cols-3 gap-3 text-sm tabular-nums">
        <Figure label="Buffer">{s.bufferSeconds.toFixed(0)} s</Figure>
        <Figure label="Fetched">
          {s.fetchedMbps.toFixed(1)}
          <span className="text-zinc-400"> / {s.bitrateMbps.toFixed(0)} Mbps</span>
        </Figure>
        <Figure label="Node">
          <span className="block truncate">{s.media.name}</span>
        </Figure>
      </dl>
    </Link>
  )
}

function Figure({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs text-zinc-500">{label}</dt>
      <dd className="mt-0.5 font-medium">{children}</dd>
    </div>
  )
}
