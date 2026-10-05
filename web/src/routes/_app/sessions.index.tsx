import { createFileRoute } from '@tanstack/react-router'
import { useStatus } from '#/api/client'
import { Buffer, Dot, Empty, NodeLink, Panel, Risk, SessionName, Table, Ago, mbps, td } from '#/components/ui'

export const Route = createFileRoute('/_app/sessions/')({ component: Sessions })

function Sessions() {
  const { data: status } = useStatus()
  if (!status) return null
  const { sessions, limits } = status

  return (
    <Panel
      title={
        <>
          Sessions <span className="ml-1 font-normal text-zinc-400">{sessions.length}</span>
        </>
      }
      flush
    >
      {sessions.length === 0 ? (
        <Empty>Nothing playing</Empty>
      ) : (
        <Table head={['Session', 'Media / standby', 'Read-ahead', 'Rate', 'Risk', 'Failovers']}>
          {sessions.map((s) => (
            <tr key={s.key} className={s.streams === 0 ? 'opacity-50' : undefined}>
              <td className={`${td} max-w-80`}>
                <SessionName s={s}>
                  <Ago iso={s.started} />
                  {s.streams === 0 && (
                    <>
                      <Dot />
                      idle
                    </>
                  )}
                </SessionName>
              </td>
              <td className={td}>
                <NodeLink {...s.media} className="block" />
                {s.standby && <NodeLink {...s.standby} className="block text-xs text-zinc-500" />}
              </td>
              <td className={`${td} w-48`}>
                <Buffer seconds={s.bufferSeconds} limits={limits} />
              </td>
              <td className={td}>
                {s.liveMbps.toFixed(1)}
                <span className="text-zinc-500"> / {mbps(s.bitrateMbps)}</span>
              </td>
              <td className={td}>
                <Risk value={s.stallRisk} target={limits.stallRisk} />
              </td>
              <td className={`${td} text-zinc-500`}>{s.failovers}</td>
            </tr>
          ))}
        </Table>
      )}
    </Panel>
  )
}
