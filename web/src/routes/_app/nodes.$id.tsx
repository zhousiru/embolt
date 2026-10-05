import { useQuery } from '@tanstack/react-query'
import { Link, createFileRoute } from '@tanstack/react-router'
import { nodeQuery, statusQuery } from '#/api/client'
import { BufferBar, Card, Empty, Range, Role, SessionName, Stat, ago, td, th } from '#/components/ui'

export const Route = createFileRoute('/_app/nodes/$id')({ component: NodeDetail })

function NodeDetail() {
  const { id } = Route.useParams()
  const { data: n, error } = useQuery(nodeQuery(id))
  const { data: status } = useQuery(statusQuery)
  if (error) return <Empty>{error.message}</Empty>
  if (!n || !status) return null
  const { limits } = status
  const sessions = status.sessions.filter((s) => s.media.id === id || s.standby?.id === id)

  return (
    <div className="space-y-4">
      <div>
        <Link to="/nodes" className="text-sm text-zinc-500 hover:underline">
          ← Nodes
        </Link>
        <h1 className="mt-1 text-xl font-semibold">{n.name}</h1>
        <div className="mt-1 text-sm text-zinc-500">
          {n.roles.map((r) => (
            <Role key={r} role={r} />
          ))}
          {n.protocol} · {n.provider || 'inline'} · <code>{n.id}</code>
        </div>
      </div>

      <div className="grid grid-cols-2 gap-4 rounded-lg border border-zinc-200 bg-white p-4 sm:grid-cols-3 dark:border-zinc-800 dark:bg-zinc-900">
        <Stat label="Rate" value={<Range e={n.rateMbps} unit="Mbps" />} />
        <Stat label="RTT" value={<Range e={n.rttMs} unit="ms" />} />
        <Stat
          label="Breaker"
          value={n.breakerOpen ? 'open' : 'closed'}
          hint={n.breakerOpen ? `until ${new Date(n.openUntil!).toLocaleTimeString()}` : undefined}
        />
      </div>

      {sessions.length > 0 && (
        <Card title="Sessions">
          <div className="-m-4 overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="border-b border-zinc-200 dark:border-zinc-800">
                <tr>
                  <th className={th}>Session</th>
                  <th className={th}>Role</th>
                  <th className={th}>Read-ahead</th>
                  <th className={th}>Rate</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-zinc-100 tabular-nums dark:divide-zinc-800">
                {sessions.map((s) => {
                  const media = s.media.id === id
                  return (
                    <tr key={s.key} className={s.streams === 0 ? 'opacity-50' : undefined}>
                      <td className={td}>
                        <SessionName s={s} />
                      </td>
                      <td className={td}>
                        <Role role={media ? 'media' : 'standby'} />
                      </td>
                      <td className={`${td} w-40`}>
                        <BufferBar seconds={s.bufferSeconds} max={limits.readAheadSeconds} low={limits.bufferMinSeconds} />
                        <div className="mt-1 text-xs text-zinc-500">{s.bufferSeconds.toFixed(0)} s</div>
                      </td>
                      <td className={td}>
                        {media ? s.liveMbps.toFixed(1) : '—'} <span className="text-zinc-500">/ {s.bitrateMbps.toFixed(1)} Mbps</span>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        </Card>
      )}

      <Card title="Recent samples">
        {n.samples.length === 0 ? (
          <Empty>No samples yet.</Empty>
        ) : (
          <div className="-m-4 overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="border-b border-zinc-200 dark:border-zinc-800">
                <tr>
                  <th className={th}>When</th>
                  <th className={th}>Kind</th>
                  <th className={th}>Rate</th>
                  <th className={th}>TTFB</th>
                  <th className={th}>Result</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-zinc-100 tabular-nums dark:divide-zinc-800">
                {n.samples.map((s, i) => (
                  <tr key={i}>
                    <td className={td}>{ago(s.time)}</td>
                    <td className={td}>{s.kind}</td>
                    <td className={td}>{s.mbps ? `${s.mbps.toFixed(1)} Mbps` : '—'}</td>
                    <td className={td}>{s.ttfbMs ? `${s.ttfbMs.toFixed(0)} ms` : '—'}</td>
                    <td className={`${td} ${s.error ? 'text-rose-600' : 'text-zinc-500'}`}>{s.error || 'ok'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </div>
  )
}
