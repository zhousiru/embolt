import { createFileRoute } from '@tanstack/react-router'
import { useStatus } from '#/api/client'
import { SessionTable } from '#/components/sessions'
import { Empty, Panel } from '#/components/ui'

export const Route = createFileRoute('/_app/sessions/')({ component: Sessions })

function Sessions() {
  const { data: status } = useStatus()
  if (!status) return null
  const { sessions, recent, limits } = status

  return (
    <div className="space-y-6">
      <Panel title="Playing" flush>
        {sessions.length === 0 ? <Empty>Nothing playing</Empty> : <SessionTable sessions={sessions} limits={limits} />}
      </Panel>
      <Panel title="Last 24 hours" flush>
        {recent.length === 0 ? <Empty>No sessions ended today</Empty> : <SessionTable sessions={recent} limits={limits} />}
      </Panel>
    </div>
  )
}
