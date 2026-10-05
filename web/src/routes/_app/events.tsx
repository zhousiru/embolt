import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { eventsQuery } from '#/api/client'
import { Card, Empty, EventList } from '#/components/ui'

export const Route = createFileRoute('/_app/events')({ component: Events })

function Events() {
  const { data: events = [] } = useQuery(eventsQuery)

  return (
    <Card title="Events">{events.length === 0 ? <Empty>No events yet.</Empty> : <EventList events={events} />}</Card>
  )
}
