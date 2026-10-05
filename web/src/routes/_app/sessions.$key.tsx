import { useQuery } from '@tanstack/react-query'
import { Link, createFileRoute } from '@tanstack/react-router'
import { sessionQuery, useLimits } from '#/api/client'
import { BufferBar, Card, Empty, EventList, Range, Risk, Role, Stat, Thumb, ago, itemTitle, td, th } from '#/components/ui'

export const Route = createFileRoute('/_app/sessions/$key')({ component: SessionDetail })

function SessionDetail() {
  const { key } = Route.useParams()
  const { data: d, error } = useQuery(sessionQuery(key))
  const limits = useLimits()
  if (error) return <Empty>{error.message}</Empty>
  if (!d) return null
  const s = d.session

  return (
    <div className="space-y-4">
      <div>
        <Link to="/sessions" className="text-sm text-zinc-500 hover:underline">
          ← Sessions
        </Link>
        <div className="mt-2 flex items-center gap-4">
          {s && <Thumb item={s.item} className="h-20" />}
          <div className="min-w-0">
            <h1 className="truncate text-xl font-semibold">{s ? itemTitle(s).title : key}</h1>
            {s?.item && (
              <div className="truncate text-sm text-zinc-600 dark:text-zinc-400">
                {itemTitle(s).detail}
                <span className="ml-2 font-mono text-xs text-zinc-500">{key}</span>
              </div>
            )}
          </div>
        </div>
        <div className="mt-1 text-sm text-zinc-500">
          {s ? (
            <>
              {s.bitrateMbps.toFixed(1)} Mbps · started {ago(s.started)} · {s.failovers} failover{s.failovers === 1 ? '' : 's'}
              {s.streams === 0 && ' · idle'}
            </>
          ) : (
            'ended'
          )}
        </div>
      </div>

      {s && (
        <div className="grid grid-cols-2 gap-4 rounded-lg border border-zinc-200 bg-white p-4 sm:grid-cols-4 dark:border-zinc-800 dark:bg-zinc-900">
          <Stat
            label="Read-ahead"
            value={`${s.bufferSeconds.toFixed(0)} s`}
            hint={<BufferBar seconds={s.bufferSeconds} max={limits.readAheadSeconds} low={limits.bufferMinSeconds} />}
          />
          <Stat label="Live rate" value={`${s.liveMbps.toFixed(1)} Mbps`} hint={`of ${s.bitrateMbps.toFixed(1)} Mbps`} />
          <Stat label="Stall risk" value={<Risk value={s.stallRisk} target={limits.stallRisk} />} />
          <Stat
            label={d.verdict?.to ? 'Switching' : 'Staying'}
            value={<span className="text-base">{d.verdict?.to?.name ?? s.media.name}</span>}
            hint={d.verdict?.reason}
          />
        </div>
      )}

      {s && d.choices.length > 0 && (
        <Card title="Node selection" aside={`at ${s.bufferSeconds.toFixed(0)} s · ${s.bitrateMbps.toFixed(1)} Mbps`}>
          <div className="-m-4 overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="border-b border-zinc-200 dark:border-zinc-800">
                <tr>
                  <th className={th}>Node</th>
                  <th className={th}>Gap</th>
                  <th className={th}>Stall risk</th>
                  <th className={th}>Exp. stall</th>
                  <th className={th}>Rate</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-zinc-100 tabular-nums dark:divide-zinc-800">
                {d.choices.map((c) => {
                  const next = c.node.id === (d.verdict?.to?.id ?? s.media.id)
                  return (
                    <tr key={c.node.id} className={next ? 'bg-sky-50 dark:bg-sky-950/40' : undefined}>
                      <td className={td}>
                        <Link to="/nodes/$id" params={{ id: c.node.id }} className="font-medium hover:underline">
                          {c.node.name}
                        </Link>
                        {c.role && (
                          <span className="ml-2">
                            <Role role={c.role} />
                          </span>
                        )}
                      </td>
                      <td className={`${td} text-zinc-500`}>{c.role === 'media' ? '—' : `${c.gapSeconds.toFixed(1)} s`}</td>
                      <td className={td}>
                        <Risk value={c.stallRisk} target={limits.stallRisk} />
                      </td>
                      <td className={td}>{c.stallSeconds.toFixed(1)} s</td>
                      <td className={td}>
                        <Range e={c.rateMbps} unit="Mbps" />
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        </Card>
      )}

      <Card title="Events">
        {d.events.length === 0 ? <Empty>No events yet.</Empty> : <EventList events={d.events} hide={['session']} />}
      </Card>
    </div>
  )
}
