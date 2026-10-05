import type { ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import type { Estimate, Event, Item, Session } from '#/api/types.gen'

export function Card({ title, children, aside }: { title: string; children: ReactNode; aside?: ReactNode }) {
  return (
    <section className="rounded-lg border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900">
      <header className="flex items-center justify-between border-b border-zinc-200 px-4 py-2.5 dark:border-zinc-800">
        <h2 className="text-sm font-semibold">{title}</h2>
        {aside && <div className="text-xs text-zinc-500">{aside}</div>}
      </header>
      <div className="p-4">{children}</div>
    </section>
  )
}

export function Stat({ label, value, hint }: { label: string; value: ReactNode; hint?: ReactNode }) {
  return (
    <div>
      <div className="text-xs uppercase tracking-wide text-zinc-500">{label}</div>
      <div className="mt-1 text-xl font-semibold tabular-nums">{value}</div>
      {hint && <div className="mt-0.5 text-xs text-zinc-500">{hint}</div>}
    </div>
  )
}

/** Stall risk against the target: green under it, amber up to 10×, red above. */
export function Risk({ value, target }: { value: number; target: number }) {
  const tone =
    value <= target
      ? 'bg-emerald-100 text-emerald-800 dark:bg-emerald-950 dark:text-emerald-300'
      : value <= 10 * target
        ? 'bg-amber-100 text-amber-800 dark:bg-amber-950 dark:text-amber-300'
        : 'bg-rose-100 text-rose-800 dark:bg-rose-950 dark:text-rose-300'
  return (
    <span title={`target ${pct(target)}`} className={`rounded px-1.5 py-0.5 text-xs font-medium tabular-nums ${tone}`}>
      {pct(value)}
    </span>
  )
}

export function Role({ role }: { role: string }) {
  return (
    <span className="mr-1 rounded bg-sky-100 px-1.5 py-0.5 text-xs font-medium text-sky-800 dark:bg-sky-950 dark:text-sky-300">
      {role}
    </span>
  )
}

/** A belief as its median, with its 90% range and evidence underneath. */
export function Range({ e, unit, digits = 0 }: { e: Estimate; unit: string; digits?: number }) {
  if (!e.measured) return <span className="text-zinc-400">—</span>
  return (
    <span className="tabular-nums">
      {e.mean.toFixed(digits)} <span className="text-zinc-500">{unit}</span>
      <span className="block text-xs text-zinc-500" title={`90% range · ${e.evidence.toFixed(1)} samples of evidence`}>
        {e.low.toFixed(digits)}–{e.high.toFixed(digits)} · n {e.evidence.toFixed(1)}
      </span>
    </span>
  )
}

/** Read-ahead fill against the ring, with the low mark. */
export function BufferBar({ seconds, max, low }: { seconds: number; max: number; low: number }) {
  const fill = Math.min(seconds / max, 1) * 100
  return (
    <div className="relative h-2 w-full overflow-hidden rounded bg-zinc-200 dark:bg-zinc-800">
      <div
        className={`h-full ${seconds < low ? 'bg-rose-500' : 'bg-emerald-500'}`}
        style={{ width: `${fill}%` }}
      />
      <div className="absolute inset-y-0 w-px bg-zinc-500" style={{ left: `${(low / max) * 100}%` }} />
    </div>
  )
}

const levelTone: Record<string, string> = {
  WARN: 'text-amber-600 dark:text-amber-400',
  ERROR: 'text-rose-600 dark:text-rose-400',
}

/** Events newest first; attrs named in hide are left out. */
export function EventList({ events, hide = [] }: { events: Event[]; hide?: string[] }) {
  return (
    <ol className="-my-1 divide-y divide-zinc-100 text-sm dark:divide-zinc-800">
      {events.map((e, i) => (
        <li key={i} className="flex gap-4 py-2">
          <time className="w-20 shrink-0 tabular-nums text-zinc-500">{new Date(e.time).toLocaleTimeString()}</time>
          <div className="min-w-0">
            <span className={`font-medium ${levelTone[e.level] ?? ''}`}>{e.message}</span>
            <div className="mt-0.5 flex flex-wrap gap-x-3 text-xs text-zinc-500">
              {Object.entries(e.attrs)
                .filter(([k]) => !hide.includes(k))
                .map(([k, v]) => (
                  <span key={k}>
                    {k}=
                    {k === 'session' ? (
                      <Link to="/sessions/$key" params={{ key: v }} className="text-zinc-700 hover:underline dark:text-zinc-300">
                        {v}
                      </Link>
                    ) : (
                      <span className="text-zinc-700 dark:text-zinc-300">{v}</span>
                    )}
                  </span>
                ))}
            </div>
          </div>
        </li>
      ))}
    </ol>
  )
}

const pad = (n: number) => String(n).padStart(2, '0')

/** What a session plays: a show with its episode, or a title and year. The key until the item is known. */
export function itemTitle(s: Session): { title: string; detail?: string } {
  const it = s.item
  if (!it) return { title: s.key }
  if (it.seriesName) {
    const ep = it.season != null && it.episode != null ? `S${pad(it.season)}E${pad(it.episode)}` : undefined
    return { title: it.seriesName, detail: [ep, it.name].filter(Boolean).join(' · ') }
  }
  return { title: it.name, detail: it.year ? String(it.year) : undefined }
}

/** The item's image, at its own aspect: an episode still or a poster. */
export function Thumb({ item, className = 'h-10' }: { item?: Item; className?: string }) {
  if (!item?.image) return null
  return (
    <img
      src={`/api/v1/items/${encodeURIComponent(item.id)}/image`}
      alt=""
      loading="lazy"
      className={`${className} w-auto shrink-0 rounded bg-zinc-200 object-cover dark:bg-zinc-800`}
    />
  )
}

/** A session's thumbnail and title, linked to its page; children go on the line below. */
export function SessionName({ s, children }: { s: Session; children?: ReactNode }) {
  const { title, detail } = itemTitle(s)
  return (
    <div className="flex min-w-0 items-center gap-3">
      <Thumb item={s.item} />
      <div className="min-w-0">
        <Link to="/sessions/$key" params={{ key: s.key }} title={s.key} className="block truncate font-medium hover:underline">
          {title}
        </Link>
        {(detail || children) && (
          <div className="truncate text-xs text-zinc-500">
            {detail}
            {detail && children && ' · '}
            {children}
          </div>
        )}
      </div>
    </div>
  )
}

export function Empty({ children }: { children: ReactNode }) {
  return <p className="text-sm text-zinc-500">{children}</p>
}

export const pct = (x: number) => (x < 0.001 ? '<0.1%' : `${(x * 100).toFixed(x < 0.1 ? 1 : 0)}%`)

export const ago = (iso: string) => {
  const s = Math.round((Date.now() - Date.parse(iso)) / 1000)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.round(s / 60)}m ago`
  return `${Math.round(s / 3600)}h ago`
}

export const th = 'whitespace-nowrap px-3 py-2 text-left text-xs font-medium uppercase tracking-wide text-zinc-500'
export const td = 'whitespace-nowrap px-3 py-2 align-top'
