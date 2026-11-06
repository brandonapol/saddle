// Waking the orchestrator (#170): the pure parts; register.tsx runs them.
// The mod reads the orchestrator's events itself (`saddle plugin events
// --json --peek`) and submits a `[saddle]` prompt only for the ones that need a
// decision. Routine ones (spawned, finished,
// landed) are toasted and attached to the person's next message, or to the
// next wake, instead of starting a turn. Events are acked (`plugin events
// --ack`) only once handled: routine ones when they join the backlog, the
// ones that need a decision after their prompt went in (#793), so a submit
// that fails leaves them pending for the next tick.
//
// It writes a heartbeat to .saddle/mod.alive each tick. While that is fresh,
// `saddle plugin hook` (the settings hooks) and `saddle plugin wait` leave the
// events to the mod, so none is delivered twice and no background wait is
// needed.
import type { SaddleSnapshot } from '../types'

/** The orchestrator's task id, whose notices the mod delivers. */
export const ORCHESTRATOR = 't0'
/** The heartbeat `saddle plugin hook` and `plugin wait` check, under the repo. */
export const HEARTBEAT = '.saddle/mod.alive'
/** Routine events kept for the next message, oldest dropped first. */
export const BACKLOG_MAX = 20

/** One orchestrator notice: `action` needs a decision, `info` does not. `id` is set under --peek. */
export type SaddleEvent = { kind: string; text: string; id?: number }

/** Parses `saddle plugin events --json`. */
export function parseEvents(text: string): { value: SaddleEvent[] } | { error: string } {
  try {
    const v = JSON.parse(text) as { events?: unknown }
    if (!v || !Array.isArray(v.events)) {
      return { error: 'saddle plugin events: no events list' }
    }
    const out: SaddleEvent[] = []
    for (const e of v.events as Array<Record<string, unknown>>) {
      if (e && typeof e.text === 'string') {
        const ev: SaddleEvent = { kind: typeof e.kind === 'string' ? e.kind : 'info', text: e.text }
        if (typeof e.id === 'number') {
          ev.id = e.id
        }
        out.push(ev)
      }
    }
    return { value: out }
  } catch (err) {
    return { error: `saddle plugin events: ${err instanceof Error ? err.message : String(err)}` }
  }
}

/** The open decision cards `saddle plugin events --json` lists, or '' (#793). */
export function parseCards(text: string): string {
  try {
    const v = JSON.parse(text) as { cards?: unknown }
    return typeof v?.cards === 'string' ? v.cards : ''
  } catch {
    return ''
  }
}

/** The ids of events, for `saddle plugin events --ack`. */
export function idsOf(events: SaddleEvent[]): number[] {
  return events.flatMap(e => (typeof e.id === 'number' ? [e.id] : []))
}

/** Whether an event wakes Claude: only the ones the orchestrator must decide. */
export function wakes(e: SaddleEvent): boolean {
  return e.kind === 'action'
}

/**
 * Engine-lock holders that leave the orchestrator's events to a session:
 * the plugin engine, and `saddle up --rc`, whose orchestrator is this
 * session under Remote Control (#409). Under plain `saddle up` they are
 * the TUI's.
 */
export const SESSION_ENGINES = ['engine', 'up-rc']

/**
 * Whether the orchestrator has events waiting: a SESSION_ENGINES holder runs
 * and status counts notices for t0. The snapshot refresh already reads
 * status, so this costs no extra command.
 */
export function hasEvents(s: SaddleSnapshot | null): boolean {
  if (!s?.isLive || !SESSION_ENGINES.includes(s.engine)) {
    return false
  }
  const t0 = s.status?.tasks.find(t => t.id === ORCHESTRATOR)
  return (t0?.pending_notices ?? 0) > 0
}

/** Whether a session with this SADDLE_TASK may host the mod: the user's own, or saddle up --rc's orchestrator. */
export function ownsSession(task: string | null | undefined): boolean {
  return !task || task === ORCHESTRATOR
}

/** How an event reads to Claude, as the settings hook delivered it. */
export function eventLine(e: SaddleEvent): string {
  return `[saddle] ${e.text}`
}

/** A routine event's toast: its first line. */
export function toastOf(e: SaddleEvent): string {
  return eventLine(e).split('\n')[0] ?? ''
}

/**
 * The prompt a wake submits: the events that need a decision, then the
 * routine backlog, then the decision cards still open, so a compacted
 * session doesn't forget them (#793).
 */
export function wakePrompt(actions: SaddleEvent[], backlog: string[], cards = ''): string {
  let text = actions.map(eventLine).join('\n\n')
  if (backlog.length) {
    text += `\n\nAlso since your last update:\n\n${backlog.join('\n\n')}`
  }
  if (cards) {
    text += `\n\n[saddle] ${cards}`
  }
  return text
}

/** Adds routine events to the backlog, keeping the newest BACKLOG_MAX. */
export function kept(backlog: string[], infos: SaddleEvent[]): string[] {
  return [...backlog, ...infos.map(eventLine)].slice(-BACKLOG_MAX)
}

/** What one batch of events does: the toasts, the prompt to submit if any, and the backlog left. */
export type WakePlan = { toasts: string[]; prompt: string | null; backlog: string[] }

/**
 * Plans one batch of taken events: routine ones toast and join the backlog;
 * when any needs a decision, one prompt carries them and the backlog, which
 * empties.
 */
export function planWake(events: SaddleEvent[], backlog: string[], cards = ''): WakePlan {
  const actions = events.filter(wakes)
  const infos = events.filter(e => !wakes(e))
  const toasts = infos.map(toastOf)
  const next = kept(backlog, infos)
  if (actions.length === 0) {
    return { toasts, prompt: null, backlog: next }
  }
  return { toasts, prompt: wakePrompt(actions, next, cards), backlog: [] }
}

/** The context the person's next message carries: the routine backlog, if any. */
export function backlogContext(backlog: string[]): string | null {
  return backlog.length ? backlog.join('\n\n') : null
}
