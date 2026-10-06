import { useLayoutEffect, useRef, useState } from 'react'
import type { Event, Limits, Point } from '#/api/types.gen'
import { clock } from './ui'

const span = 10 * 60_000 // ms shown: the server keeps 10 min of steps
const gapAfter = 6_000 // ms between steps that breaks the line: the session was idle

/** The width of the element, kept current. */
function useWidth() {
  const ref = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(0)
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const ro = new ResizeObserver(([e]) => setWidth(e.contentRect.width))
    ro.observe(el)
    return () => ro.disconnect()
  }, [])
  return [ref, width] as const
}

/** An SVG path through the points, broken where steps are missing. */
function line(pts: Point[], x: (p: Point) => number, y: (p: Point) => number) {
  let d = ''
  pts.forEach((p, i) => {
    const gap = i === 0 || Date.parse(p.at) - Date.parse(pts[i - 1].at) > gapAfter
    d += `${gap ? 'M' : 'L'}${x(p).toFixed(1)},${y(p).toFixed(1)}`
  })
  return d
}

/** The same path closed down to base, for an area fill. */
function area(pts: Point[], x: (p: Point) => number, y: (p: Point) => number, base: number) {
  const runs: Point[][] = []
  pts.forEach((p, i) => {
    if (i === 0 || Date.parse(p.at) - Date.parse(pts[i - 1].at) > gapAfter) runs.push([])
    runs[runs.length - 1].push(p)
  })
  return runs
    .map((r) => `${line(r, x, y)}L${x(r[r.length - 1]).toFixed(1)},${base}L${x(r[0]).toFixed(1)},${base}Z`)
    .join('')
}

/** A round axis top at or above x. */
function nice(x: number) {
  const steps = [10, 20, 30, 60, 90, 120, 180, 240, 300, 500, 1000]
  return steps.find((s) => s >= x) ?? Math.ceil(x / 1000) * 1000
}

interface Marker {
  t: number
  kind: 'switch' | 'test'
  label: string
}

/** Node switches and speed tests, from the session's events. */
function markers(events: Event[]): Marker[] {
  return events.flatMap((e): Marker[] => {
    const t = Date.parse(e.time)
    if (e.message === 'switched') return [{ t, kind: 'switch', label: `Switched to ${e.attrs.to} (${e.attrs.reason})` }]
    if (e.message === 'explored') return [{ t, kind: 'test', label: `Tested ${e.attrs.node}: ${e.attrs.mbps} Mbps` }]
    return []
  })
}

/**
 * A session's last 10 minutes: the buffer against the low mark and, unless
 * compact, the rate fetched from upstream against the bitrate. Switches and
 * speed tests are marked; hovering reads off a step.
 */
export function SessionChart({
  history,
  events = [],
  limits,
  bitrate,
  compact,
}: {
  history: Point[]
  events?: Event[]
  limits: Limits
  bitrate: number
  compact?: boolean
}) {
  const [ref, width] = useWidth()
  const [hover, setHover] = useState<number>()

  const end = history.length ? Date.parse(history[history.length - 1].at) : Date.now()
  const start = end - span
  const pts = history.filter((p) => Date.parse(p.at) >= start)
  const marks = markers(events).filter((m) => m.t >= start && m.t <= end + 2_000)

  const left = compact ? 0 : 40
  const w = Math.max(0, width - left)
  const bufH = compact ? 48 : 140
  const rateTop = bufH + 20
  const rateH = compact ? 0 : 64
  const height = compact ? bufH : rateTop + rateH + 20

  const bufMax = nice(Math.max(limits.readAheadSeconds, ...pts.map((p) => p.buffer)))
  const rateMax = Math.max(bitrate * 1.5, ...pts.map((p) => p.mbps), 1) * 1.05
  const x = (p: Point | number) => left + ((typeof p === 'number' ? p : Date.parse(p.at)) - start) / span * w
  const yBuf = (p: Point | number) => bufH - (Math.min(typeof p === 'number' ? p : p.buffer, bufMax) / bufMax) * (bufH - 2)
  const yRate = (p: Point | number) => rateTop + rateH - ((typeof p === 'number' ? p : p.mbps) / rateMax) * rateH

  const at = hover === undefined ? undefined : pts[hover]
  const near = at && marks.find((m) => Math.abs(x(m.t) - x(at)) < 6)

  function move(e: React.PointerEvent<SVGSVGElement>) {
    if (!pts.length) return
    const t = start + ((e.clientX - e.currentTarget.getBoundingClientRect().left - left) / w) * span
    let best = 0
    pts.forEach((p, i) => {
      if (Math.abs(Date.parse(p.at) - t) < Math.abs(Date.parse(pts[best].at) - t)) best = i
    })
    setHover(best)
  }

  return (
    <div ref={ref} className="relative">
      {width > 0 && (
        <svg width={width} height={height} className="block overflow-visible" onPointerMove={move} onPointerLeave={() => setHover(undefined)}>
          {!compact && (
            <g className="fill-zinc-400 text-[11px] tabular-nums dark:fill-zinc-500">
              <text x={0} y={10}>{bufMax} s</text>
              <text x={0} y={yBuf(limits.bufferMinSeconds) + 4}>{limits.bufferMinSeconds} s</text>
              <text x={0} y={rateTop + 10}>{Math.round(rateMax)}</text>
              <text x={0} y={rateTop + 22}>Mbps</text>
              <text x={left} y={height - 2}>−10 min</text>
              <text x={left + w / 2} y={height - 2} textAnchor="middle">−5 min</text>
              <text x={left + w} y={height - 2} textAnchor="end">{clock(new Date(end).toISOString())}</text>
            </g>
          )}
          <line x1={left} x2={left + w} y1={bufH} y2={bufH} className="stroke-zinc-200 dark:stroke-zinc-800" />
          <line x1={left} x2={left + w} y1={yBuf(limits.bufferMinSeconds)} y2={yBuf(limits.bufferMinSeconds)} strokeDasharray="4 3" className="stroke-rose-400/70" />
          <path d={area(pts, x, yBuf, bufH)} className="fill-sky-500/15" />
          <path d={line(pts, x, yBuf)} fill="none" strokeWidth={1.5} strokeLinejoin="round" className="stroke-sky-500" />
          {!compact && (
            <>
              <line x1={left} x2={left + w} y1={rateTop + rateH} y2={rateTop + rateH} className="stroke-zinc-200 dark:stroke-zinc-800" />
              <line x1={left} x2={left + w} y1={yRate(bitrate)} y2={yRate(bitrate)} strokeDasharray="4 3" className="stroke-zinc-400/70" />
              <path d={line(pts, x, yRate)} fill="none" strokeWidth={1.5} strokeLinejoin="round" className="stroke-emerald-500" />
            </>
          )}
          {marks.map((m, i) => (
            <line
              key={i}
              x1={x(m.t)}
              x2={x(m.t)}
              y1={0}
              y2={compact ? bufH : rateTop + rateH}
              strokeDasharray={m.kind === 'test' ? '2 3' : undefined}
              className={m.kind === 'switch' ? 'stroke-amber-500' : 'stroke-zinc-400 dark:stroke-zinc-500'}
            />
          ))}
          {at && (
            <g>
              <line x1={x(at)} x2={x(at)} y1={0} y2={compact ? bufH : rateTop + rateH} className="stroke-zinc-300 dark:stroke-zinc-600" />
              <circle cx={x(at)} cy={yBuf(at)} r={3} className="fill-sky-500" />
              {!compact && <circle cx={x(at)} cy={yRate(at)} r={3} className="fill-emerald-500" />}
            </g>
          )}
        </svg>
      )}
      {at && (
        <div
          className="pointer-events-none absolute top-0 z-10 rounded-md bg-white px-2.5 py-1.5 text-xs whitespace-nowrap tabular-nums shadow-md ring-1 ring-zinc-200 dark:bg-zinc-800 dark:ring-zinc-700"
          style={x(at) > width / 2 ? { right: width - x(at) + 8 } : { left: x(at) + 8 }}
        >
          <div className="text-zinc-500">{clock(at.at)}</div>
          <div>
            <span className="text-sky-600 dark:text-sky-400">{at.buffer.toFixed(0)} s</span> buffered
          </div>
          <div>
            <span className="text-emerald-600 dark:text-emerald-400">{at.mbps.toFixed(1)} Mbps</span> fetched
          </div>
          {near && <div className="mt-1 text-amber-600 dark:text-amber-400">{near.label}</div>}
        </div>
      )}
      {!pts.length && <p className="absolute inset-0 flex items-center justify-center text-xs text-zinc-400">Waiting for the first step</p>}
    </div>
  )
}

/** The chart's key, for the full chart. */
export function ChartKey() {
  const item = (swatch: string, label: string) => (
    <span className="inline-flex items-center gap-1.5">
      <span className={swatch} />
      {label}
    </span>
  )
  return (
    <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-zinc-500">
      {item('h-0.5 w-3 rounded bg-sky-500', 'Buffer')}
      {item('h-0.5 w-3 rounded bg-emerald-500', 'Fetched')}
      {item('w-3 border-t border-dashed border-zinc-400', 'Bitrate')}
      {item('w-3 border-t border-dashed border-rose-400', 'Low mark')}
      {item('h-3 w-0.5 bg-amber-500', 'Switch')}
      {item('h-3 border-l border-dashed border-zinc-400', 'Speed test')}
    </div>
  )
}
