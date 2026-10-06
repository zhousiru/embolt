import { Link, createFileRoute } from '@tanstack/react-router'
import { useLimits, useSession } from '#/api/client'
import { Badge, Buffer, Dot, Empty, EventList, NodeLink, Panel, PageHeader, Range, Headroom, Role, Stat, Stats, Table, Thumb, Ago, itemTitle, mbps, td } from '#/components/ui'

export const Route = createFileRoute('/_app/sessions/$key')({ component: SessionDetail })

function SessionDetail() {
  const { key } = Route.useParams()
  const { data: d } = useSession(key)
  const limits = useLimits()
  if (d === null) return <Empty>Session not found</Empty>
  if (!d) return null
  const s = d.session
  const name = s ? itemTitle(s) : { title: key }
  const next = d.verdict?.to?.id ?? s?.media.id

  return (
    <div className="space-y-6">
      <PageHeader
        back={
          <Link to="/sessions" className="hover:text-zinc-900 dark:hover:text-zinc-100">
            ← Sessions
          </Link>
        }
        media={s?.item?.image && <Thumb item={s.item} className="h-20" />}
        title={name.title}
        meta={
          s ? (
            <>
              {name.detail && (
                <>
                  <span className="text-zinc-700 dark:text-zinc-300">{name.detail}</span>
                  <Dot />
                </>
              )}
              <span>{mbps(s.bitrateMbps)}</span>
              <Dot />
              <Ago iso={s.started} />
              <Dot />
              <span>
                {s.failovers} failover{s.failovers === 1 ? '' : 's'}
              </span>
              {s.streams === 0 && (
                <>
                  <Dot />
                  <Badge>idle</Badge>
                </>
              )}
            </>
          ) : (
            <Badge>ended</Badge>
          )
        }
      />

      {s && (
        <Stats>
          <Stat label="Read-ahead">
            <div className="pt-1.5">
              <Buffer seconds={s.bufferSeconds} limits={limits} />
            </div>
          </Stat>
          <Stat label="Live rate" sub={`of ${mbps(s.bitrateMbps)}`}>
            {mbps(s.liveMbps)}
          </Stat>
          <Stat label="Headroom" sub={`${mbps(s.safeMbps)} safe of ${mbps(s.needMbps)}`}>
            <Headroom safe={s.safeMbps} need={s.needMbps} />
          </Stat>
          <Stat label={d.verdict?.to ? 'Switching to' : 'Staying on'} sub={d.verdict?.reason}>
            <NodeLink {...(d.verdict?.to ?? s.media)} />
          </Stat>
        </Stats>
      )}

      {s && d.choices.length > 0 && (
        <Panel title="Node choice" flush>
          <Table head={['Node', 'Gap', 'Headroom', 'Safe / need', 'Rate']}>
            {d.choices.map((c) => (
              <tr key={c.node.id} className={c.node.id === next ? 'bg-sky-50/70 dark:bg-sky-400/5' : undefined}>
                <td className={td}>
                  <div className="flex items-center gap-2">
                    <NodeLink {...c.node} className="font-medium" />
                    {c.role && <Role role={c.role} />}
                  </div>
                </td>
                <td className={`${td} text-zinc-500`}>{c.role === 'media' ? '—' : `${c.gapSeconds.toFixed(1)} s`}</td>
                <td className={td}>
                  <Headroom safe={c.safeMbps} need={c.needMbps} known={c.known} />
                </td>
                <td className={td}>
                  {mbps(c.safeMbps)} <span className="text-zinc-500">/ {mbps(c.needMbps)}</span>
                </td>
                <td className={td}>
                  <Range e={c.rateMbps} unit="Mbps" />
                </td>
              </tr>
            ))}
          </Table>
        </Panel>
      )}

      <Panel title="Events" flush>
        <div className="border-t border-zinc-100 dark:border-zinc-800">
          {d.events.length === 0 ? <Empty>No events</Empty> : <EventList events={d.events} hide={['session']} />}
        </div>
      </Panel>
    </div>
  )
}
