import { useSyncExternalStore } from 'react'
import type { ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import type { Estimate, Event, Item, Limits, Session } from '#/api/types.gen'

export function Panel({
  title,
  aside,
  flush,
  children,
}: {
  title?: ReactNode
  aside?: ReactNode
  flush?: boolean
  children: ReactNode
}) {
  return (
    <section className="overflow-hidden rounded-xl bg-white ring-1 ring-zinc-200/80 dark:bg-zinc-900 dark:ring-zinc-800">
      {title && (
        <header className="flex min-h-12 items-center justify-between gap-4 px-5 py-2">
          <h2 className="text-sm font-semibold">{title}</h2>
          {aside && <div className="text-sm text-zinc-500">{aside}</div>}
        </header>
      )}
      <div className={flush ? undefined : 'px-5 pb-5'}>{children}</div>
    </section>
  )
}

export function Stats({ children }: { children: ReactNode }) {
  return (
    <div className="overflow-hidden rounded-xl bg-white ring-1 ring-zinc-200/80 dark:bg-zinc-900 dark:ring-zinc-800">
      {/* Each stat draws its left and top border; the -1px margin tucks the outer ones under the clip. */}
      <div className="-mt-px -ml-px flex flex-wrap">{children}</div>
    </div>
  )
}

export function Stat({ label, children, sub }: { label: string; children: ReactNode; sub?: ReactNode }) {
  return (
    <div className="min-w-0 flex-1 basis-40 border-t border-l border-zinc-200/80 px-5 py-4 dark:border-zinc-800">
      <div className="text-xs font-medium text-zinc-500">{label}</div>
      <div className="mt-1.5 truncate text-lg font-semibold tabular-nums">{children}</div>
      {sub && <div className="mt-1 truncate text-xs text-zinc-500">{sub}</div>}
    </div>
  )
}

const tones = {
  neutral: 'bg-zinc-100 text-zinc-700 dark:bg-zinc-800 dark:text-zinc-300',
  accent: 'bg-sky-50 text-sky-700 dark:bg-sky-400/10 dark:text-sky-300',
  good: 'bg-emerald-50 text-emerald-700 dark:bg-emerald-400/10 dark:text-emerald-300',
  warn: 'bg-amber-50 text-amber-700 dark:bg-amber-400/10 dark:text-amber-300',
  bad: 'bg-rose-50 text-rose-700 dark:bg-rose-400/10 dark:text-rose-300',
}

export function Badge({ tone = 'neutral', title, children }: { tone?: keyof typeof tones; title?: string; children: ReactNode }) {
  return (
    <span title={title} className={`inline-flex items-center rounded-md px-1.5 py-0.5 text-xs font-medium tabular-nums ${tones[tone]}`}>
      {children}
    </span>
  )
}

export const Role = ({ role }: { role: string }) => <Badge tone="accent">{role}</Badge>

/** Stall risk against the target: good under it, warn up to 10×, bad above. */
export function Risk({ value, target }: { value: number; target: number }) {
  const tone = value <= target ? 'good' : value <= 10 * target ? 'warn' : 'bad'
  return (
    <Badge tone={tone} title={`target ${pct(target)}`}>
      {pct(value)}
    </Badge>
  )
}

/** A belief as its median, with its 90% range and evidence underneath. */
export function Range({ e, unit, digits = 0 }: { e: Estimate; unit: string; digits?: number }) {
  if (!e.measured) return <span className="text-zinc-400 dark:text-zinc-600">—</span>
  return (
    <span className="tabular-nums">
      {e.mean.toFixed(digits)}
      <span className="ml-1 text-xs text-zinc-500">{unit}</span>
      <span className="block text-xs text-zinc-400 dark:text-zinc-500" title={`90% range · n ${e.evidence.toFixed(1)}`}>
        {e.low.toFixed(digits)}–{e.high.toFixed(digits)}
      </span>
    </span>
  )
}

/** Read-ahead fill against the ring, with the low mark. */
export function Buffer({ seconds, limits }: { seconds: number; limits: Limits }) {
  const max = limits.readAheadSeconds
  const low = limits.bufferMinSeconds
  return (
    <div className="flex items-center gap-3">
      <div className="relative h-1.5 w-full min-w-20 overflow-hidden rounded-full bg-zinc-100 dark:bg-zinc-800">
        <div
          className={`h-full rounded-full transition-[width] duration-700 ease-out ${seconds < low ? 'bg-rose-500' : 'bg-emerald-500'}`}
          style={{ width: `${Math.min(seconds / max, 1) * 100}%` }}
        />
        <div className="absolute inset-y-0 w-px bg-zinc-400/70" style={{ left: `${(low / max) * 100}%` }} />
      </div>
      <span className="w-8 shrink-0 text-right text-xs tabular-nums text-zinc-500">{seconds.toFixed(0)}s</span>
    </div>
  )
}

export function Table({ head, children }: { head: ReactNode[]; children: ReactNode }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-y border-zinc-100 bg-zinc-50/60 dark:border-zinc-800 dark:bg-zinc-950/30">
            {head.map((h, i) => (
              <th key={i} className="whitespace-nowrap px-5 py-2 text-left text-xs font-medium text-zinc-500">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y divide-zinc-100 tabular-nums dark:divide-zinc-800">{children}</tbody>
      </table>
    </div>
  )
}

export const td = 'whitespace-nowrap px-5 py-3 align-middle'

const levelDot: Record<string, string> = {
  WARN: 'bg-amber-500',
  ERROR: 'bg-rose-500',
}

/** Events newest first; attrs named in hide are left out. */
export function EventList({ events, hide = [] }: { events: Event[]; hide?: string[] }) {
  return (
    <ol className="divide-y divide-zinc-100 text-sm dark:divide-zinc-800">
      {events.map((e, i) => (
        <li key={`${e.time}-${i}`} className="flex gap-4 px-5 py-3">
          <time className="w-16 shrink-0 pt-px text-xs tabular-nums text-zinc-500" dateTime={e.time} title={new Date(e.time).toLocaleString()}>
            {new Date(e.time).toLocaleTimeString([], { hour12: false })}
          </time>
          <span className={`mt-1.5 size-1.5 shrink-0 rounded-full ${levelDot[e.level] ?? 'bg-zinc-300 dark:bg-zinc-600'}`} />
          <div className="min-w-0">
            <div className="font-medium">{e.message}</div>
            <div className="mt-1 flex flex-wrap gap-1.5">
              {Object.entries(e.attrs)
                .filter(([k]) => !hide.includes(k))
                .map(([k, v]) => (
                  <span key={k} className="rounded bg-zinc-100 px-1.5 py-0.5 font-mono text-[11px] text-zinc-600 dark:bg-zinc-800 dark:text-zinc-400">
                    <span className="text-zinc-400 dark:text-zinc-500">{k}</span>{' '}
                    {k === 'session' ? (
                      <Link to="/sessions/$key" params={{ key: v }} className="hover:underline">
                        {v}
                      </Link>
                    ) : (
                      v
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
export function itemTitle(s: Session): { title: string; detail?: ReactNode } {
  const it = s.item
  if (!it) return { title: s.key }
  if (it.seriesName) {
    const ep = it.season != null && it.episode != null ? `S${pad(it.season)}E${pad(it.episode)}` : undefined
    return { title: it.seriesName, detail: ep ? <>{ep}<Dot />{it.name}</> : it.name }
  }
  return { title: it.name, detail: it.year ? String(it.year) : undefined }
}

/** The item's image, at its own aspect: an episode still or a poster. */
export function Thumb({ item, className = 'h-10' }: { item?: Item; className?: string }) {
  if (!item?.image) return <div className={`${className} aspect-video shrink-0 rounded-md bg-zinc-100 dark:bg-zinc-800`} />
  return (
    <img
      src={`/api/v1/items/${encodeURIComponent(item.id)}/image`}
      alt=""
      loading="lazy"
      className={`${className} w-auto shrink-0 rounded-md bg-zinc-100 object-cover ring-1 ring-black/5 dark:bg-zinc-800 dark:ring-white/5`}
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
            {detail && children && <Dot />}
            {children}
          </div>
        )}
      </div>
    </div>
  )
}

export const NodeLink = ({ id, name, className = '' }: { id: string; name: string; className?: string }) => (
  <Link to="/nodes/$id" params={{ id }} className={`hover:underline ${className}`}>
    {name}
  </Link>
)

export function PageHeader({ back, title, meta, media }: { back?: ReactNode; title: ReactNode; meta?: ReactNode; media?: ReactNode }) {
  return (
    <div className="flex items-center gap-4">
      {media}
      <div className="min-w-0">
        {back && <div className="mb-1 text-sm text-zinc-500">{back}</div>}
        <h1 className="truncate text-2xl font-semibold tracking-tight">{title}</h1>
        {meta && <div className="mt-1.5 flex flex-wrap items-center gap-x-0.5 gap-y-1 text-sm text-zinc-500">{meta}</div>}
      </div>
    </div>
  )
}

export function Empty({ children }: { children: ReactNode }) {
  return <p className="px-5 py-10 text-center text-sm text-zinc-400 dark:text-zinc-500">{children}</p>
}

/** A separator drawn, not typed, so it never lands in a selection or a copy. */
export const Dot = () => (
  <span aria-hidden className="mx-1.5 inline-block size-[3px] shrink-0 rounded-full bg-current align-middle opacity-40" />
)

export const pct = (x: number) => (x < 0.001 ? '<0.1%' : `${(x * 100).toFixed(x < 0.1 ? 1 : 0)}%`)

export const mbps = (x: number, digits = 1) => `${x.toFixed(digits)} Mbps`

export const clock = (iso: string) => new Date(iso).toLocaleTimeString([], { hour12: false })

const ago = (iso: string, now: number) => {
  const s = Math.max(0, Math.round((now - Date.parse(iso)) / 1000))
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.round(s / 60)}m`
  if (s < 86400) return `${Math.round(s / 3600)}h`
  return `${Math.round(s / 86400)}d`
}

// One shared one-second clock: streams send only on change, so relative
// times tick on their own.
const ticks = new Set<() => void>()
let clock1s: ReturnType<typeof setInterval> | undefined
function onTick(f: () => void) {
  ticks.add(f)
  clock1s ??= setInterval(() => ticks.forEach((g) => g()), 1_000)
  return () => {
    ticks.delete(f)
    if (ticks.size === 0) clock1s = void clearInterval(clock1s)
  }
}

/** Time since iso, kept current. */
export function Ago({ iso }: { iso: string }) {
  const now = useSyncExternalStore(onTick, () => Math.floor(Date.now() / 1000) * 1000, () => Date.parse(iso))
  return <span title={new Date(iso).toLocaleString()}>{ago(iso, now)}</span>
}
