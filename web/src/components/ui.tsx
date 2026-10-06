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

const states: Record<string, { label: string; tone: keyof typeof tones; title: string }> = {
  starting: { label: 'Starting', tone: 'accent', title: 'Opening its stream' },
  ok: { label: 'Smooth', tone: 'good', title: 'Its node delivers the bitrate, or the read-ahead holds' },
  risk: { label: 'Behind', tone: 'warn', title: 'Its node delivers under the bitrate and the read-ahead is running down' },
  low: { label: 'Draining', tone: 'bad', title: 'Behind with the read-ahead under the low mark: the player is on its own buffer' },
  idle: { label: 'Idle', tone: 'neutral', title: 'No stream open' },
  ended: { label: 'Ended', tone: 'neutral', title: '' },
}

/** A session's state as of its last step. */
export function State({ state }: { state: string }) {
  const st = states[state] ?? { label: state, tone: 'neutral', title: '' }
  return (
    <Badge tone={st.tone} title={st.title || undefined}>
      {st.label}
    </Badge>
  )
}

/** An estimate as its moving average. */
export function Range({ e, unit, digits = 0 }: { e: Estimate; unit: string; digits?: number }) {
  if (!e.measured) return <span className="text-zinc-400 dark:text-zinc-600">—</span>
  return (
    <span className="tabular-nums">
      {e.mean.toFixed(digits)}
      <span className="ml-1 text-xs text-zinc-500">{unit}</span>
    </span>
  )
}

/** A rate's moving average, drawn on a scale shared by its column. */
export function RateBar({ e, scale }: { e: Estimate; scale: number }) {
  if (!e.measured) return <span className="text-zinc-400 dark:text-zinc-600">—</span>
  return (
    <div className="flex items-center gap-3">
      <span className="w-20 shrink-0 tabular-nums">
        {e.mean.toFixed(0)}
        <span className="ml-1 text-xs text-zinc-500">Mbps</span>
      </span>
      <div className="relative h-1.5 w-full min-w-24 rounded-full bg-zinc-100 dark:bg-zinc-800">
        <div className="absolute inset-y-0 left-0 rounded-full bg-sky-500" style={{ width: `${Math.min(e.mean / scale, 1) * 100}%` }} />
      </div>
    </div>
  )
}

/** Seconds of read-ahead against its length, with the low mark; red while the session is behind. */
export function Ahead({ seconds, behind, limits }: { seconds: number; behind: boolean; limits: Limits }) {
  const max = limits.readAheadSeconds
  const low = limits.lowMarkSeconds
  return (
    <div className="flex items-center gap-3">
      <div className="relative h-1.5 w-full min-w-20 overflow-hidden rounded-full bg-zinc-100 dark:bg-zinc-800">
        <div
          className={`h-full rounded-full transition-[width] duration-700 ease-out ${behind ? 'bg-rose-500' : 'bg-emerald-500'}`}
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

/** A session's thumbnail and title, linked to its page unless link is false; children go on the line below. */
export function SessionName({ s, link = true, children }: { s: Session; link?: boolean; children?: ReactNode }) {
  const { title, detail } = itemTitle(s)
  return (
    <div className="flex min-w-0 items-center gap-3">
      <Thumb item={s.item} />
      <div className="min-w-0">
        {link ? (
          <Link to="/sessions/$key" params={{ key: s.key }} title={s.key} className="block truncate font-medium hover:underline">
            {title}
          </Link>
        ) : (
          <div title={s.key} className="truncate font-medium">
            {title}
          </div>
        )}
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


export const mbps = (x: number, digits = 1) => `${x.toFixed(digits)} Mbps`

/** A span of seconds, rounded to what matters: 42 s, 38 min, 1 h 52 min. */
export function duration(seconds: number) {
  const s = Math.round(seconds)
  if (s < 60) return `${s} s`
  const m = Math.round(s / 60)
  if (m < 60) return `${m} min`
  return m % 60 ? `${Math.floor(m / 60)} h ${m % 60} min` : `${m / 60} h`
}

/** How long a session has run, or ran. */
export const length = (s: Session) => duration(((s.ended ? Date.parse(s.ended) : Date.now()) - Date.parse(s.started)) / 1000)

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
