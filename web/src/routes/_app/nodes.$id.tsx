import { Link, createFileRoute } from '@tanstack/react-router'
import { useLimits, useNode, useStatus } from '#/api/client'
import { SessionTable } from '#/components/sessions'
import { Badge, Dot, Empty, Panel, PageHeader, Range, Role, Stat, Stats, Table, Ago, clock, mbps, td } from '#/components/ui'

export const Route = createFileRoute('/_app/nodes/$id')({ component: NodeDetail })

function NodeDetail() {
  const { id } = Route.useParams()
  const { data: n } = useNode(id)
  const { data: status } = useStatus()
  const limits = useLimits()
  if (n === null) return <Empty>Node not found</Empty>
  if (!n) return null
  const sessions = (status?.sessions ?? []).filter((s) => s.media.id === id)

  return (
    <div className="space-y-6">
      <PageHeader
        back={
          <Link to="/nodes" className="hover:text-zinc-900 dark:hover:text-zinc-100">
            ← Nodes
          </Link>
        }
        title={n.name}
        meta={
          <>
            <span className="inline-flex gap-1">
              {n.roles.map((r) => (
                <Role key={r} role={r} />
              ))}
            </span>
            {n.roles.length > 0 && <Dot />}
            <span>{n.protocol}</span>
            <Dot />
            <span>{n.provider || 'inline'}</span>
            <Dot />
            <code className="text-xs">{n.id}</code>
          </>
        }
      />

      <Stats>
        <Stat label="Rate">
          <Range e={n.rateMbps} unit="Mbps" />
        </Stat>
        <Stat label="RTT">
          <Range e={n.rttMs} unit="ms" />
        </Stat>
        <Stat label="Last sample">{n.sampled ? <><Ago iso={n.sampled} /> ago</> : <span className="text-zinc-400">never</span>}</Stat>
        <Stat label="Breaker">
          {n.breakerOpen ? <Badge tone="bad">open to {clock(n.openUntil!)}</Badge> : <Badge tone="good">closed</Badge>}
        </Stat>
      </Stats>

      {sessions.length > 0 && (
        <Panel title="Playing on this node" flush>
          <SessionTable sessions={sessions} limits={limits} />
        </Panel>
      )}

      <Panel title="Samples" flush>
        {n.samples.length === 0 ? (
          <Empty>No samples</Empty>
        ) : (
          <Table head={['When', 'Kind', 'Rate', 'TTFB', 'Result']}>
            {n.samples.map((s, i) => (
              <tr key={i}>
                <td className={`${td} text-zinc-500`}>
                  <Ago iso={s.time} />
                </td>
                <td className={td}>{s.kind}</td>
                <td className={td}>{s.mbps ? mbps(s.mbps) : '—'}</td>
                <td className={td}>{s.ttfbMs ? `${s.ttfbMs.toFixed(0)} ms` : '—'}</td>
                <td className={td}>{s.error ? <span className="text-rose-600 dark:text-rose-400">{s.error}</span> : <Badge tone="good">ok</Badge>}</td>
              </tr>
            ))}
          </Table>
        )}
      </Panel>
    </div>
  )
}
