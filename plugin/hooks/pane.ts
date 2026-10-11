// The saddle pane's rows (#167): what each tab shows of the shared snapshot,
// as plain lines the render hook in register.tsx draws. The pane reads the
// snapshot only; nothing here runs saddle.
import type { SaddlePaneTab, SaddleSnapshot, SaddleStackNode, SaddleTask } from '../types'

/** How a line is drawn: dim, a warning, a fault, good news, a heading. */
export type Tone = 'dim' | 'warn' | 'bad' | 'good' | 'head'

export type Line = { text: string; tone?: Tone }

/** The tabs in the order drawn, with the hotkey that shows each. */
export const TABS: readonly { tab: SaddlePaneTab; label: string; hotkey: string }[] = [
  { tab: 'agents', label: 'Agents', hotkey: '1' },
  { tab: 'claims', label: 'Claims', hotkey: '2' },
  { tab: 'train', label: 'Train', hotkey: '3' },
  { tab: 'stacks', label: 'Stacks', hotkey: '4' },
]

/** Rows the pane keeps above its scrolled body: the header and the tabs. */
export const PINNED_ROWS = 2

const FINISHED = new Set(['landed', 'killed'])
// Tasks no longer working: the header's active count leaves them out.
const STOPPED = new Set([...FINISHED, 'failed'])

function tasksOf(s: SaddleSnapshot): SaddleTask[] {
  return s.status?.tasks ?? []
}

/** The pane's one-line summary above the tabs. */
export function headerOf(s: SaddleSnapshot | null): string {
  if (!s) {
    return 'saddle · reading…'
  }
  if (!s.isLive) {
    return 'saddle · not running'
  }
  const tasks = tasksOf(s)
  const parts = ['saddle', s.engine, `${tasks.filter(t => !STOPPED.has(t.status)).length} active`]
  const needs = tasks.filter(t => t.status === 'needs_you').length
  if (needs > 0) {
    parts.push(`${needs} needs you`)
  }
  parts.push(`${s.queue.length} in train`)
  return parts.join(' · ')
}

// notLive is what every tab shows before saddle has been read, or while it
// isn't running; undefined once there is something to draw.
function notLive(s: SaddleSnapshot | null): Line[] | undefined {
  if (!s) {
    return [{ text: 'Reading saddle…', tone: 'dim' }]
  }
  if (!s.isLive) {
    return [{ text: "saddle isn't running: start `saddle plugin engine` or `saddle up`.", tone: 'dim' }]
  }
  return undefined
}

function taskTone(status: string): Tone | undefined {
  switch (status) {
    case 'needs_you':
    case 'idle':
    case 'paused':
      return 'warn'
    case 'failed':
    case 'conflict':
    case 'orphaned':
      return 'bad'
    case 'done':
      return 'good'
    case 'landed':
    case 'killed':
      return 'dim'
    default:
      return undefined
  }
}

// lastActivity is the best word the snapshot has for what a task did last.
function lastActivity(t: SaddleTask): string | undefined {
  if (t.activity) {
    return t.activity
  }
  if (t.reason) {
    return t.reason
  }
  if (t.train) {
    return `train: ${t.train}`
  }
  if (t.pending_notices) {
    return `${t.pending_notices} notice${t.pending_notices === 1 ? '' : 's'} waiting`
  }
  return undefined
}

function width(values: string[]): number {
  return Math.max(0, ...values.map(v => v.length))
}

/** Agents: id, state, model and title, the last activity under each. */
export function agentLines(s: SaddleSnapshot | null): Line[] {
  const early = notLive(s)
  if (early || !s) {
    return early ?? []
  }
  const tasks = tasksOf(s)
  if (tasks.length === 0) {
    return [{ text: 'No agents yet.', tone: 'dim' }]
  }
  const ordered = [...tasks.filter(t => !FINISHED.has(t.status)), ...tasks.filter(t => FINISHED.has(t.status))]
  const idW = width(ordered.map(t => t.id))
  // At least as wide as the longest status saddle has, so columns hold still.
  const stW = Math.max(width(ordered.map(t => t.status)), 'needs_you'.length + 2)
  const modelW = width(ordered.map(t => t.model || '-'))
  const out: Line[] = []
  for (const t of ordered) {
    const tone = taskTone(t.status)
    const text = [t.id.padEnd(idW), t.status.padEnd(stW), (t.model || '-').padEnd(modelW), t.title].join('  ')
    out.push(tone ? { text, tone } : { text })
    const act = lastActivity(t)
    if (act) {
      out.push({ text: `${' '.repeat(idW + 2)}${act}`, tone: 'dim' })
    }
  }
  return out
}

/** Claims: each live task's globs. */
export function claimLines(s: SaddleSnapshot | null): Line[] {
  const early = notLive(s)
  if (early || !s) {
    return early ?? []
  }
  const holders = tasksOf(s).filter(t => !FINISHED.has(t.status) && (t.claims?.length ?? 0) > 0)
  if (holders.length === 0) {
    return [{ text: 'No claims held.', tone: 'dim' }]
  }
  const idW = width(holders.map(t => t.id))
  const out: Line[] = []
  for (const t of holders) {
    ;(t.claims ?? []).forEach((glob, i) => {
      out.push({ text: `${(i === 0 ? t.id : '').padEnd(idW)}  ${glob}` })
    })
  }
  return out
}

function trainTone(state: string): Tone | undefined {
  switch (state) {
    case 'on_hold':
      return 'warn'
    case 'conflict':
    case 'test_failed':
      return 'bad'
    default:
      return undefined
  }
}

/** Train: the merge queue next first, then the auto-merge holds. */
export function trainLines(s: SaddleSnapshot | null): Line[] {
  const early = notLive(s)
  if (early || !s) {
    return early ?? []
  }
  const out: Line[] = []
  const idW = width(s.queue.map(q => q.task))
  for (const q of s.queue) {
    const parts = [`${q.position}. ${q.task.padEnd(idW)}`, q.state]
    if (q.note) {
      parts.push(q.note)
    }
    if (q.attempts && q.attempts > 1) {
      parts.push(`(${q.attempts} attempts)`)
    }
    const tone = trainTone(q.state)
    const text = parts.join('  ')
    out.push(tone ? { text, tone } : { text })
  }
  if (out.length === 0) {
    out.push({ text: 'The train is empty.', tone: 'dim' })
  }
  const holds = s.stack?.holds ?? []
  if (holds.length > 0) {
    out.push({ text: `Auto-merge holds: ${holds.join(', ')}`, tone: 'warn' })
  }
  return out
}

const CHECKS: Record<string, string> = {
  ChecksPass: 'pass',
  ChecksFail: 'fail',
  ChecksPending: 'pending',
  ChecksNone: 'none',
}

function nodeLine(n: SaddleStackNode, i: number): Line {
  const head = `  ${i + 1}. ${n.task} ${n.pr} → ${n.base}`
  if (n.error) {
    return { text: `${head}  can't read: ${n.error}`, tone: 'bad' }
  }
  const tags = [`checks ${CHECKS[n.checks ?? ''] ?? (n.checks || 'unknown')}`]
  if (n.mergeable) {
    tags.push(n.mergeable.toLowerCase())
  }
  if (n.merge_state) {
    tags.push(n.merge_state.toLowerCase())
  }
  if (n.draft) {
    tags.push('draft')
  }
  if (n.labels?.length) {
    tags.push(`labels ${n.labels.join(',')}`)
  }
  if (n.at_risk) {
    tags.push(`at risk: ${n.at_risk}`)
  }
  const isBad = n.checks === 'ChecksFail' || n.mergeable === 'CONFLICTING' || !!n.at_risk
  const text = `${head}  ${tags.join(' · ')}`
  return isBad ? { text, tone: 'bad' } : { text }
}

/** Stacks: the auto-merge watcher, then each PR stack bottom first. */
export function stackLines(s: SaddleSnapshot | null): Line[] {
  const early = notLive(s)
  if (early || !s) {
    return early ?? []
  }
  const st = s.stack
  if (!st) {
    return [{ text: 'Stacks not read yet.', tone: 'dim' }]
  }
  const out: Line[] = []
  const watcher = [`Auto-merge ${st.enabled ? 'on' : 'off'} (${st.source})`]
  if (st.busy) {
    watcher.push('train busy')
  }
  if (st.holds?.length) {
    watcher.push(`holds: ${st.holds.join(', ')}`)
  }
  out.push({ text: watcher.join(' · '), tone: 'dim' })
  if (st.stopped) {
    out.push({ text: `Stopped: ${st.stopped}. \`saddle automerge on\` resumes it.`, tone: 'bad' })
  }
  const stacks = st.stacks ?? []
  if (stacks.length === 0) {
    out.push({ text: 'No open PR stacks.', tone: 'dim' })
    return out
  }
  for (const k of stacks) {
    const tags = [`stack ${k.id}`]
    if (k.held) {
      tags.push('held')
    }
    if (k.behind) {
      tags.push(`${k.behind} behind base`)
    }
    out.push({ text: tags.join(' · '), tone: 'head' })
    k.nodes.forEach((n, i) => out.push(nodeLine(n, i)))
    if (k.ready) {
      const text = `  next: ${k.next} is ready to merge${k.blocked ? `; ${k.blocked}` : ''}`
      out.push({ text, tone: k.blocked ? 'warn' : 'good' })
    } else {
      out.push({ text: `  next: ${k.next} waits: ${k.why ?? 'unknown'}`, tone: 'warn' })
    }
  }
  return out
}

/** One tab's rows, led by whatever the last refresh could not read. */
export function tabLines(s: SaddleSnapshot | null, tab: SaddlePaneTab): Line[] {
  const errors: Line[] = (s?.errors ?? []).map(text => ({ text: `! ${text}`, tone: 'bad' }))
  switch (tab) {
    case 'agents':
      return [...errors, ...agentLines(s)]
    case 'claims':
      return [...errors, ...claimLines(s)]
    case 'train':
      return [...errors, ...trainLines(s)]
    case 'stacks':
      return [...errors, ...stackLines(s)]
  }
}

/**
 * The first row shown after moving `by` rows from `offset`, over `total`
 * rows in a window of `window`: never past the top, nor so far the window
 * shows less than it could.
 */
export function scrolled(offset: number, by: number, total: number, window: number): number {
  return Math.max(0, Math.min(offset + by, total - window))
}
