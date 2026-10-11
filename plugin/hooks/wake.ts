// Waking the orchestrator (#170): the pure parts; register.tsx runs them.
// The mod takes the orchestrator's events itself (`saddle plugin events
// --json`) and submits a `[saddle]` prompt only for the ones that need a
// decision. Routine ones (spawned, finished,
// landed) are toasted and attached to the person's next message, or to the
// next wake, instead of starting a turn.
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

/** One orchestrator notice: `action` needs a decision, `info` does not. */
export type SaddleEvent = { kind: string; text: string }

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
        out.push({ kind: typeof e.kind === 'string' ? e.kind : 'info', text: e.text })
      }
    }
    return { value: out }
  } catch (err) {
    return { error: `saddle plugin events: ${err instanceof Error ? err.message : String(err)}` }
  }
}

/** Whether an event wakes Claude: only the ones the orchestrator must decide. */
export function wakes(e: SaddleEvent): boolean {
  return e.kind === 'action'
}

/**
 * Whether the orchestrator has events waiting: the plugin engine runs (under
 * `saddle up` they are the TUI's) and status counts notices for t0. The
 * snapshot refresh already reads status, so this costs no extra command.
 */
export function hasEvents(s: SaddleSnapshot | null): boolean {
  if (!s?.isLive || s.engine !== 'engine') {
    return false
  }
  const t0 = s.status?.tasks.find(t => t.id === ORCHESTRATOR)
  return (t0?.pending_notices ?? 0) > 0
}

/** How an event reads to Claude, as the settings hook delivered it. */
export function eventLine(e: SaddleEvent): string {
  return `[saddle] ${e.text}`
}

/** A routine event's toast: its first line. */
export function toastOf(e: SaddleEvent): string {
  return eventLine(e).split('\n')[0] ?? ''
}

/** The prompt a wake submits: the events that need a decision, then the routine backlog. */
export function wakePrompt(actions: SaddleEvent[], backlog: string[]): string {
  let text = actions.map(eventLine).join('\n\n')
  if (backlog.length) {
    text += `\n\nAlso since your last update:\n\n${backlog.join('\n\n')}`
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
export function planWake(events: SaddleEvent[], backlog: string[]): WakePlan {
  const actions = events.filter(wakes)
  const infos = events.filter(e => !wakes(e))
  const toasts = infos.map(toastOf)
  const next = kept(backlog, infos)
  if (actions.length === 0) {
    return { toasts, prompt: null, backlog: next }
  }
  return { toasts, prompt: wakePrompt(actions, next), backlog: [] }
}

/** The context the person's next message carries: the routine backlog, if any. */
export function backlogContext(backlog: string[]): string | null {
  return backlog.length ? backlog.join('\n\n') : null
}
