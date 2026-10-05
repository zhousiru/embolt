import { useQuery } from '@tanstack/react-query'
import { Link, createFileRoute } from '@tanstack/react-router'
import { statusQuery, useLimits } from '#/api/client'
import { BufferBar, Card, Empty, Risk, SessionName, ago, td, th } from '#/components/ui'

export const Route = createFileRoute('/_app/sessions/')({ component: Sessions })

function Sessions() {
  const { data: status } = useQuery(statusQuery)
  const sessions = status?.sessions ?? []
  const limits = useLimits()

  return (
    <Card title="Sessions">
      {sessions.length === 0 ? (
        <Empty>Nothing is playing.</Empty>
      ) : (
        <div className="-m-4 overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="border-b border-zinc-200 dark:border-zinc-800">
              <tr>
                <th className={th}>Session</th>
                <th className={th}>Media / standby</th>
                <th className={th}>Read-ahead</th>
                <th className={th}>Rate</th>
                <th className={th}>Stall risk</th>
                <th className={th}>Failovers</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-zinc-100 tabular-nums dark:divide-zinc-800">
              {sessions.map((s) => (
                <tr key={s.key} className={s.streams === 0 ? 'opacity-50' : undefined}>
                  <td className={td}>
                    <SessionName s={s}>
                      {ago(s.started)}
                      {s.streams === 0 && ' · idle'}
                    </SessionName>
                  </td>
                  <td className={td}>
                    <Link to="/nodes/$id" params={{ id: s.media.id }} className="hover:underline">
                      {s.media.name}
                    </Link>
                    {s.standby && (
                      <Link to="/nodes/$id" params={{ id: s.standby.id }} className="block text-xs text-zinc-500 hover:underline">
                        {s.standby.name}
                      </Link>
                    )}
                  </td>
                  <td className={`${td} w-40`}>
                    <BufferBar seconds={s.bufferSeconds} max={limits.readAheadSeconds} low={limits.bufferMinSeconds} />
                    <div className="mt-1 text-xs text-zinc-500">{s.bufferSeconds.toFixed(0)} s</div>
                  </td>
                  <td className={td}>
                    {s.liveMbps.toFixed(1)} <span className="text-zinc-500">/ {s.bitrateMbps.toFixed(1)} Mbps</span>
                  </td>
                  <td className={td}>
                    <Risk value={s.stallRisk} target={limits.stallRisk} />
                  </td>
                  <td className={td}>{s.failovers}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  )
}
