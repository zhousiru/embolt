import { useCallback, useSyncExternalStore } from 'react'
import type { Event, Limits, Node, NodeDetail, SessionDetail, Status } from './types.gen'

/** A resource's latest snapshot: undefined until the first lands, null once the server says it is gone. */
export interface Snapshot<T> {
  data: T | null | undefined
  live: boolean
}

interface Poll {
  snap: Snapshot<unknown>
  body?: string
  subs: Set<() => void>
  timer?: ReturnType<typeof setTimeout>
  abort?: AbortController
}

// One poll per path, shared by every component that reads it. The next
// request goes out a second after the last one settles, so requests never
// pile up; polling stops when the last reader leaves, and the snapshot stays
// for the next one.
const polls = new Map<string, Poll>()
const every = 1_000
const initial: Snapshot<never> = { data: undefined, live: false }

async function tick(path: string, p: Poll) {
  p.abort = new AbortController()
  let snap: Snapshot<unknown>
  try {
    const res = await fetch(`/api/v1/${path}`, { signal: p.abort.signal })
    if (res.status === 404) {
      snap = { data: null, live: true }
      p.body = undefined
    } else if (!res.ok) {
      snap = { ...p.snap, live: false }
    } else {
      const body = await res.text()
      // An unchanged body keeps the old snapshot, so readers skip a render.
      snap = body === p.body && p.snap.live ? p.snap : { data: JSON.parse(body), live: true }
      p.body = body
    }
  } catch {
    if (p.abort.signal.aborted) return
    snap = { ...p.snap, live: false }
  }
  if (snap !== p.snap) {
    p.snap = snap
    p.subs.forEach((f) => f())
  }
  if (p.subs.size > 0) p.timer = setTimeout(() => tick(path, p), every)
}

function subscribe(path: string, notify: () => void) {
  const p = polls.get(path) ?? { snap: initial, subs: new Set() }
  polls.set(path, p)
  if (p.subs.size === 0) void tick(path, p)
  p.subs.add(notify)
  return () => {
    p.subs.delete(notify)
    if (p.subs.size === 0) {
      clearTimeout(p.timer)
      p.abort?.abort()
    }
  }
}

function usePoll<T>(path: string): Snapshot<T> {
  return useSyncExternalStore(
    useCallback((notify: () => void) => subscribe(path, notify), [path]),
    () => (polls.get(path)?.snap ?? initial) as Snapshot<T>,
    () => initial,
  )
}

export const useStatus = () => usePoll<Status>('status')
export const useNodes = () => usePoll<Node[]>('nodes')
export const useNode = (id: string) => usePoll<NodeDetail>(`nodes/${encodeURIComponent(id)}`)
export const useSession = (key: string) => usePoll<SessionDetail>(`sessions/${encodeURIComponent(key)}`)
export const useEvents = () => usePoll<Event[]>('events')

const defaultLimits: Limits = { bufferMinSeconds: 10, readAheadSeconds: 60 }

export const useLimits = () => useStatus().data?.limits ?? defaultLimits
