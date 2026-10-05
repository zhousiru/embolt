import { useQuery } from '@tanstack/react-query'
import { Link, createFileRoute } from '@tanstack/react-router'
import { nodesQuery, statusQuery } from '#/api/client'
import { BufferBar, Card, Empty, Risk, SessionName, Stat, ago } from '#/components/ui'

export const Route = createFileRoute('/_app/')({ component: Overview })

function Overview() {
  const { data: status, error } = useQuery(statusQuery)
  const { data: nodes = [] } = useQuery(nodesQuery)
  if (error) return <Empty>Cannot reach Embolt: {error.message}</Empty>
  if (!status) return null
  const { limits } = status
  const open = nodes.filter((n) => n.breakerOpen)

  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-4 rounded-lg border border-zinc-200 bg-white p-4 sm:grid-cols-4 dark:border-zinc-800 dark:bg-zinc-900">
        <Stat label="Upstream" value={<span className="text-base">{status.upstream}</span>} hint={`${status.version} · up ${ago(status.started).replace(' ago', '')}`} />
        <Stat
          label="Primary"
          value={
            status.primary ? (
              <Link to="/nodes/$id" params={{ id: status.primary.id }} className="text-base hover:underline">
                {status.primary.name}
              </Link>
            ) : (
              '—'
            )
          }
        />
        <Stat label="Usable nodes" value={`${status.usable} / ${status.nodes}`} />
        <Stat label="Playing" value={status.sessions.filter((s) => s.streams > 0).length} />
      </div>

      <Card title="Sessions">
        {status.sessions.length === 0 ? (
          <Empty>Nothing is playing.</Empty>
        ) : (
          <ul className="space-y-3">
            {status.sessions.map((s) => (
              <li key={s.key} className="grid grid-cols-[1fr_auto] items-center gap-x-4 gap-y-1.5">
                <div className="min-w-0 text-sm">
                  <SessionName s={s}>
                    {s.media.name}
                    {s.standby && ` · standby ${s.standby.name}`}
                  </SessionName>
                </div>
                <Risk value={s.stallRisk} target={limits.stallRisk} />
                <BufferBar seconds={s.bufferSeconds} max={limits.readAheadSeconds} low={limits.bufferMinSeconds} />
                <span className="text-xs tabular-nums text-zinc-500">
                  {s.bufferSeconds.toFixed(0)} s · {s.liveMbps.toFixed(0)}/{s.bitrateMbps.toFixed(0)} Mbps
                </span>
              </li>
            ))}
          </ul>
        )}
      </Card>

      {open.length > 0 && (
        <Card title={`Open breakers · ${open.length}`}>
          <ul className="space-y-1 text-sm">
            {open.map((n) => (
              <li key={n.id} className="flex justify-between">
                <Link to="/nodes/$id" params={{ id: n.id }} className="hover:underline">
                  {n.name}
                </Link>
                <span className="text-zinc-500">until {new Date(n.openUntil!).toLocaleTimeString()}</span>
              </li>
            ))}
          </ul>
        </Card>
      )}
    </div>
  )
}
