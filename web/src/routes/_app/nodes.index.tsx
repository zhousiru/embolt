import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link, createFileRoute } from '@tanstack/react-router'
import { nodesQuery } from '#/api/client'
import { Card, Range, Role, td, th } from '#/components/ui'

export const Route = createFileRoute('/_app/nodes/')({ component: Nodes })

function Nodes() {
  const { data: nodes = [] } = useQuery(nodesQuery)
  const [filter, setFilter] = useState('')
  const shown = nodes.filter((n) =>
    [n.name, n.protocol, n.provider].some((f) => f.toLowerCase().includes(filter.toLowerCase())),
  )

  return (
    <Card
      title="Nodes"
      aside={
        <input
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          placeholder="Filter"
          className="w-64 rounded border border-zinc-300 bg-transparent px-2 py-1 text-sm dark:border-zinc-700"
        />
      }
    >
      <div className="-m-4 overflow-x-auto">
        <table className="w-full text-sm">
          <thead className="border-b border-zinc-200 dark:border-zinc-800">
            <tr>
              <th className={th}>Node</th>
              <th className={th}>Rate</th>
              <th className={th}>RTT</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-zinc-100 dark:divide-zinc-800">
            {shown.map((n) => (
              <tr key={n.id} className={n.breakerOpen ? 'opacity-50' : undefined}>
                <td className={td}>
                  <Link to="/nodes/$id" params={{ id: n.id }} className="font-medium hover:underline">
                    {n.name}
                  </Link>
                  <div className="mt-0.5 text-xs text-zinc-500">
                    {n.roles.map((r) => (
                      <Role key={r} role={r} />
                    ))}
                    {n.protocol} · {n.provider || 'inline'}
                    {n.breakerOpen && ' · breaker open'}
                  </div>
                </td>
                <td className={td}>
                  <Range e={n.rateMbps} unit="Mbps" />
                </td>
                <td className={td}>
                  <Range e={n.rttMs} unit="ms" />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  )
}
