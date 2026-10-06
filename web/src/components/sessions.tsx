import type { Limits, Session } from '#/api/types.gen'
import { Ago, Ahead, NodeLink, SessionName, State, Table, duration, length, td } from './ui'

const quiet = <span className="text-zinc-400">—</span>

/** Sessions playing, or ended ones with when they ended. */
export function SessionTable({ sessions, limits }: { sessions: Session[]; limits: Limits }) {
  const ended = sessions.length > 0 && sessions.every((s) => s.ended)
  return (
    <Table head={['Session', ended ? 'Ended' : 'State', 'Node', ...(ended ? [] : ['Read-ahead']), 'Length', 'Failovers', 'Draining']}>
      {sessions.map((s) => (
        <tr key={`${s.key}-${s.ended ?? ''}`}>
          <td className={`${td} max-w-80`}>
            <SessionName s={s} />
          </td>
          <td className={td}>
            {s.ended ? (
              <span className="text-zinc-500">
                <Ago iso={s.ended} /> ago
              </span>
            ) : (
              <State state={s.state} />
            )}
          </td>
          <td className={td}>
            <NodeLink {...s.media} />
          </td>
          {!ended && (
            <td className={`${td} w-48`}>
              <Ahead seconds={s.aheadSeconds} behind={s.state === 'risk' || s.state === 'low'} limits={limits} />
            </td>
          )}
          <td className={td}>{length(s)}</td>
          <td className={td}>{s.failovers || quiet}</td>
          <td className={td}>
            {s.lowSeconds >= 1 ? <span className="text-rose-600 dark:text-rose-400">{duration(s.lowSeconds)}</span> : quiet}
          </td>
        </tr>
      ))}
    </Table>
  )
}
