import { useState } from 'react'
import { createFileRoute } from '@tanstack/react-router'
import { useNodes } from '#/api/client'
import { Ago, Badge, Empty, NodeLink, Panel, RateBar, Role, Table, clock, td } from '#/components/ui'

export const Route = createFileRoute('/_app/nodes/')({ component: Nodes })

function Nodes() {
  const { data } = useNodes()
  const [filter, setFilter] = useState('')
  const nodes = data ?? []
  const q = filter.toLowerCase()
  const shown = nodes.filter((n) => [n.name, n.protocol, n.provider].some((f) => f.toLowerCase().includes(q)))
  const scale = Math.max(1, ...nodes.filter((n) => n.rateMbps.measured).map((n) => n.rateMbps.mean))

  return (
    <Panel
      title={
        <>
          Nodes <span className="ml-1 font-normal text-zinc-400">{nodes.length}</span>
        </>
      }
      aside={
        <input
          type="search"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          placeholder="Filter"
          className="w-44 rounded-md bg-zinc-100 px-3 py-1.5 text-sm text-zinc-900 outline-none placeholder:text-zinc-400 focus:ring-2 focus:ring-sky-500/40 sm:w-64 dark:bg-zinc-800 dark:text-zinc-100"
        />
      }
      flush
    >
      {shown.length === 0 ? (
        <Empty>{data ? 'No nodes' : ''}</Empty>
      ) : (
        <Table head={['Node', 'Rate', 'RTT', 'Last sample', '']}>
          {shown.map((n) => (
            <tr key={n.id} className="hover:bg-zinc-50 dark:hover:bg-zinc-800/40">
              <td className={td}>
                <div className="flex items-center gap-2">
                  <NodeLink id={n.id} name={n.name} className={`font-medium ${n.breakerOpen ? 'text-zinc-400' : ''}`} />
                  {n.roles.map((r) => (
                    <Role key={r} role={r} />
                  ))}
                </div>
              </td>
              <td className={`${td} w-64`}>
                <RateBar e={n.rateMbps} scale={scale} />
              </td>
              <td className={td}>{n.rttMs.measured ? `${n.rttMs.mean.toFixed(0)} ms` : <span className="text-zinc-400">—</span>}</td>
              <td className={`${td} text-zinc-500`}>{n.sampled ? <><Ago iso={n.sampled} /> ago</> : <span className="text-zinc-400">never</span>}</td>
              <td className={`${td} text-right`}>
                {n.breakerOpen && (
                  <Badge tone="bad" title={`Open to ${clock(n.openUntil!)}`}>
                    open
                  </Badge>
                )}
              </td>
            </tr>
          ))}
        </Table>
      )}
    </Panel>
  )
}
