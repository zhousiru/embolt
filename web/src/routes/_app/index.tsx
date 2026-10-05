import { createFileRoute } from '@tanstack/react-router'
import { useNodes, useStatus } from '#/api/client'
import { Badge, Buffer, Empty, NodeLink, Panel, Risk, SessionName, Stat, Stats, Ago, clock, mbps } from '#/components/ui'

export const Route = createFileRoute('/_app/')({ component: Overview })

function Overview() {
  const { data: status } = useStatus()
  const { data: nodes = [] } = useNodes()
  if (!status) return null
  const { limits } = status
  const open = (nodes ?? []).filter((n) => n.breakerOpen)
  const playing = status.sessions.filter((s) => s.streams > 0).length

  return (
    <div className="space-y-6">
      <Stats>
        <Stat label="Upstream" sub={<>up <Ago iso={status.started} /></>}>
          {status.upstream}
        </Stat>
        <Stat label="Primary">{status.primary ? <NodeLink {...status.primary} /> : '—'}</Stat>
        <Stat label="Nodes">
          {status.usable}
          <span className="text-zinc-400"> / {status.nodes}</span>
        </Stat>
        <Stat label="Playing">{playing}</Stat>
      </Stats>

      <Panel title="Sessions" flush>
        {status.sessions.length === 0 ? (
          <Empty>Nothing playing</Empty>
        ) : (
          <ul className="divide-y divide-zinc-100 border-t border-zinc-100 dark:divide-zinc-800 dark:border-zinc-800">
            {status.sessions.map((s) => (
              <li
                key={s.key}
                className={`grid items-center gap-x-6 gap-y-3 px-5 py-4 sm:grid-cols-[minmax(0,1fr)_14rem_auto] ${s.streams === 0 ? 'opacity-50' : ''}`}
              >
                <SessionName s={s}>
                  <NodeLink {...s.media} />
                  {s.standby && (
                    <>
                      {' → '}
                      <NodeLink {...s.standby} />
                    </>
                  )}
                </SessionName>
                <div>
                  <Buffer seconds={s.bufferSeconds} limits={limits} />
                  <div className="mt-1 text-xs tabular-nums text-zinc-500">
                    {s.liveMbps.toFixed(1)} / {mbps(s.bitrateMbps)}
                  </div>
                </div>
                <div className="sm:justify-self-end">
                  <Risk value={s.stallRisk} target={limits.stallRisk} />
                </div>
              </li>
            ))}
          </ul>
        )}
      </Panel>

      {open.length > 0 && (
        <Panel title="Open breakers" aside={<Badge tone="bad">{open.length}</Badge>} flush>
          <ul className="divide-y divide-zinc-100 border-t border-zinc-100 text-sm dark:divide-zinc-800 dark:border-zinc-800">
            {open.map((n) => (
              <li key={n.id} className="flex justify-between gap-4 px-5 py-3">
                <NodeLink id={n.id} name={n.name} className="truncate" />
                <span className="shrink-0 tabular-nums text-zinc-500">until {clock(n.openUntil!)}</span>
              </li>
            ))}
          </ul>
        </Panel>
      )}
    </div>
  )
}
