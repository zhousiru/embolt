import { useState } from 'react'
import { createFileRoute } from '@tanstack/react-router'
import { useEvents } from '#/api/client'
import { Empty, EventList, Panel } from '#/components/ui'

export const Route = createFileRoute('/_app/events')({ component: Events })

const levels = ['All', 'WARN', 'ERROR'] as const

function Events() {
  const { data } = useEvents()
  const [level, setLevel] = useState<(typeof levels)[number]>('All')
  const events = (data ?? []).filter((e) => level === 'All' || e.level === level)

  return (
    <Panel
      title="Events"
      aside={
        <div className="flex rounded-md bg-zinc-100 p-0.5 dark:bg-zinc-800">
          {levels.map((l) => (
            <button
              key={l}
              onClick={() => setLevel(l)}
              className={`rounded px-2.5 py-1 text-xs font-medium transition-colors ${
                l === level ? 'bg-white text-zinc-900 shadow-sm dark:bg-zinc-700 dark:text-zinc-100' : 'text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100'
              }`}
            >
              {l === 'All' ? 'All' : l[0] + l.slice(1).toLowerCase()}
            </button>
          ))}
        </div>
      }
      flush
    >
      <div className="border-t border-zinc-100 dark:border-zinc-800">
        {events.length === 0 ? <Empty>{data ? 'No events' : ''}</Empty> : <EventList events={events} />}
      </div>
    </Panel>
  )
}
