import { queryOptions, useQuery } from '@tanstack/react-query'
import type { Event, Limits, Node, NodeDetail, SessionDetail, Status } from './types.gen'

async function get<T>(path: string): Promise<T> {
  const res = await fetch(`/api/v1/${path}`)
  if (!res.ok) throw new Error(`${path}: ${res.status} ${res.statusText}`)
  return res.json()
}

export const statusQuery = queryOptions({
  queryKey: ['status'],
  queryFn: () => get<Status>('status'),
  refetchInterval: 2_000,
})

const defaultLimits: Limits = { stallRisk: 0.01, bufferMinSeconds: 10, readAheadSeconds: 60 }

/** The control limits, from the status poll; defaults until it lands. */
export const useLimits = () => useQuery({ ...statusQuery, select: (s) => s.limits }).data ?? defaultLimits

export const nodesQuery = queryOptions({
  queryKey: ['nodes'],
  queryFn: () => get<Node[]>('nodes'),
  refetchInterval: 10_000,
})

export const nodeQuery = (id: string) =>
  queryOptions({
    queryKey: ['nodes', id],
    queryFn: () => get<NodeDetail>(`nodes/${encodeURIComponent(id)}`),
    refetchInterval: 5_000,
  })

export const sessionQuery = (key: string) =>
  queryOptions({
    queryKey: ['sessions', key],
    queryFn: () => get<SessionDetail>(`sessions/${encodeURIComponent(key)}`),
    refetchInterval: 2_000,
  })

export const eventsQuery = queryOptions({
  queryKey: ['events'],
  queryFn: () => get<Event[]>('events'),
  refetchInterval: 5_000,
})
