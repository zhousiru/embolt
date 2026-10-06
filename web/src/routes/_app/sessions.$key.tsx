import { Link, createFileRoute } from '@tanstack/react-router'
import { useLimits, useSession } from '#/api/client'
import { ChartKey, SessionChart } from '#/components/chart'
import { Dot, Empty, EventList, NodeLink, Panel, PageHeader, State, Stat, Stats, Thumb, duration, itemTitle, length, mbps } from '#/components/ui'

export const Route = createFileRoute('/_app/sessions/$key')({ component: SessionDetail })

function SessionDetail() {
  const { key } = Route.useParams()
  const { data: d } = useSession(key)
  const limits = useLimits()
  if (d === null) return <Empty>Session not found</Empty>
  if (!d) return null
  const s = d.session
  const name = s ? itemTitle(s) : { title: key }

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
          <>
            {name.detail && (
              <>
                <span className="text-zinc-700 dark:text-zinc-300">{name.detail}</span>
                <Dot />
              </>
            )}
            {s && <span>{mbps(s.bitrateMbps, 0)}</span>}
            <span className="ml-2">
              <State state={s?.state ?? 'ended'} />
            </span>
          </>
        }
      />

      {s && (
        <Stats>
          {s.ended ? (
            <Stat label="Length">{length(s)}</Stat>
          ) : (
            <Stat label="Read-ahead">{s.aheadSeconds.toFixed(0)} s</Stat>
          )}
          {!s.ended && (
            <Stat label="Fetched">
              {s.fetchedMbps.toFixed(1)}
              <span className="text-zinc-400"> / {mbps(s.bitrateMbps, 0)}</span>
            </Stat>
          )}
          <Stat label="Node" sub={s.nodeMbps > 0 ? `${mbps(s.nodeMbps, 0)} typical` : undefined}>
            <NodeLink {...s.media} />
          </Stat>
          <Stat label="Failovers">{s.failovers}</Stat>
          <Stat label="Draining">
            {s.lowSeconds >= 1 ? <span className="text-rose-600 dark:text-rose-400">{duration(s.lowSeconds)}</span> : '—'}
          </Stat>
        </Stats>
      )}

      {s && (
        <Panel title={s.ended ? 'Last 10 minutes played' : 'Last 10 minutes'}>
          <SessionChart history={d.history} events={d.events} limits={limits} bitrate={s.bitrateMbps} />
          <div className="mt-4">
            <ChartKey />
          </div>
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
