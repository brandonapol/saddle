// The needs-you band (#168): what in the shared snapshot waits on a person or
// the orchestrator, one item per task, and what each of its buttons does.
// register.tsx draws the band and runs the commands; nothing here runs saddle.
import type { SaddleCIRed, SaddleQueueEntry, SaddleSnapshot, SaddleStackGraph, SaddleStackNode, SaddleTask } from '../types'

/**
 * Why a task needs someone: waiting on a prompt, stopped without calling
 * done, conflicted, CI failing (in the train or on its PR), flagged at risk
 * by the stack sentinel, or held in the train.
 */
export type NeedKind = 'prompt' | 'stalled' | 'conflict' | 'ci' | 'at_risk' | 'held'

/** A band button, in the order drawn. */
export type NeedAction = 'peek' | 'ask' | 'land' | 'hold' | 'release'

export type Need = {
  task: string
  title: string
  kind: NeedKind
  /** What it needs, in a few words. */
  what: string
  actions: NeedAction[]
  /** Where hold and release act: its train entry, or its PR stack. */
  via?: 'queue' | 'automerge'
}

export const ACTION_LABELS: Record<NeedAction, string> = {
  peek: 'Peek',
  ask: 'Ask Claude',
  land: 'Land',
  hold: 'Hold',
  release: 'Release',
}

/** Lines of a task's terminal Peek shows. */
export const PEEK_LINES = 15
/** The longest title a band line keeps before cutting it with an ellipsis. */
const TITLE_MAX = 40

const FINISHED = new Set(['landed', 'killed'])

// detail is what follows "state: " in a task's train field or a queue note.
function detail(train: string | undefined, q: SaddleQueueEntry | undefined): string {
  const fromTrain = train?.includes(': ') ? train.slice(train.indexOf(': ') + 2) : ''
  return fromTrain || q?.note || ''
}

function trainState(t: SaddleTask | undefined, q: SaddleQueueEntry | undefined): string {
  return q?.state ?? t?.train?.split(':')[0] ?? ''
}

function prName(pr: string): string {
  const m = pr.match(/\/(pull\/\d+)$/)
  return m?.[1] ?? pr
}

function ciRedWhat(c: SaddleCIRed): string {
  let what = `CI red on ${prName(c.pr)}`
  if (c.checks?.length) {
    what += `: ${c.checks.join(', ')}`
  }
  const more: string[] = []
  if (c.repair) {
    more.push(`${c.repair} repairing${c.attempts ? `, attempt ${c.attempts}` : ''}`)
  }
  if (c.held?.length) {
    more.push(`holds ${c.held.join(', ')}`)
  }
  return more.length ? `${what} (${more.join('; ')})` : what
}

type Facts = {
  t?: SaddleTask
  q?: SaddleQueueEntry
  red?: SaddleCIRed
  node?: SaddleStackNode
  graph?: SaddleStackGraph
}

// need says what one task needs, most pressing first, or undefined. A
// finished task can still need someone for its PR: red CI or at risk.
function need(f: Facts): Pick<Need, 'kind' | 'what'> | undefined {
  const { t, q, red, node } = f
  const isLive = !!t && !FINISHED.has(t.status)
  const state = trainState(t, q)
  if (isLive && t.status === 'needs_you') {
    return { kind: 'prompt', what: t.reason ? `waiting on a prompt: ${t.reason}` : 'waiting on a prompt' }
  }
  if (isLive && state === 'test_failed') {
    return { kind: 'ci', what: 'tests failed in the train' }
  }
  if (isLive && (t.status === 'conflict' || state === 'conflict')) {
    const why = detail(t.train, q)
    return { kind: 'conflict', what: why ? `conflicted: ${why}` : 'conflicted' }
  }
  if (red) {
    return { kind: 'ci', what: ciRedWhat(red) }
  }
  if (node?.at_risk) {
    return { kind: 'at_risk', what: `at risk: ${node.at_risk} (${node.pr})` }
  }
  if (node?.checks === 'ChecksFail') {
    return { kind: 'ci', what: `CI failing on ${node.pr}` }
  }
  if (isLive && t.status === 'idle') {
    return { kind: 'stalled', what: 'stopped without calling done' }
  }
  if (state === 'on_hold') {
    const why = detail(t?.train, q)
    return { kind: 'held', what: why ? `held in the train: ${why}` : 'held in the train' }
  }
  return undefined
}

function actionsOf(f: Facts): Pick<Need, 'actions' | 'via'> {
  const { t, q, graph } = f
  const actions: NeedAction[] = []
  if (t?.window && !FINISHED.has(t.status)) {
    actions.push('peek')
  }
  actions.push('ask')
  if (q) {
    if (q.state === 'on_hold') {
      actions.push('release')
    } else {
      actions.push('land', 'hold')
    }
    return { actions, via: 'queue' }
  }
  if (graph) {
    actions.push(graph.held ? 'release' : 'hold')
    return { actions, via: 'automerge' }
  }
  return { actions }
}

function shortTitle(title: string): string {
  return title.length > TITLE_MAX ? `${title.slice(0, TITLE_MAX - 1)}…` : title
}

/** What needs someone, one item per task, in the order status lists them. */
export function needsYou(s: SaddleSnapshot | null): Need[] {
  if (!s?.isLive) {
    return []
  }
  const tasks = s.status?.tasks ?? []
  const facts = new Map<string, Facts>()
  const at = (id: string) => {
    let f = facts.get(id)
    if (!f) {
      f = {}
      facts.set(id, f)
    }
    return f
  }
  for (const t of tasks) {
    // The orchestrator has no branch: it is the one being asked, not a worker.
    if (t.branch) {
      at(t.id).t = t
    }
  }
  for (const q of s.queue) {
    at(q.task).q = q
  }
  for (const red of s.status?.ci_red ?? []) {
    at(red.task).red = red
  }
  for (const graph of s.stack?.stacks ?? []) {
    for (const node of graph.nodes) {
      Object.assign(at(node.task), { node, graph })
    }
  }
  const out: Need[] = []
  for (const [task, f] of facts) {
    const n = need(f)
    if (n) {
      out.push({ task, title: f.t?.title ?? '', ...n, ...actionsOf(f) })
    }
  }
  return out
}

/** The band's line for an item: `‼ t54 (title): what it needs`. */
export function needLine(n: Need): string {
  return `‼ ${n.task}${n.title ? ` (${shortTitle(n.title)})` : ''}: ${n.what}`
}

/** The saddle arguments a button runs; undefined for Ask Claude, a prompt. */
export function actionArgv(n: Need, action: NeedAction): string[] | undefined {
  switch (action) {
    case 'peek':
      return ['peek', n.task, '-n', String(PEEK_LINES)]
    case 'land':
      return ['land']
    case 'hold':
      return n.via === 'automerge' ? ['automerge', 'hold', n.task] : ['queue', 'hold', n.task, 'held from the needs-you band']
    case 'release':
      return n.via === 'automerge' ? ['automerge', 'release', n.task] : ['queue', 'release', n.task]
    case 'ask':
      return undefined
  }
}

/** The `[saddle]` prompt Ask Claude submits to the orchestrating session. */
export function askPrompt(n: Need): string {
  const who = `${n.task}${n.title ? ` (${n.title})` : ''}`
  return (
    `[saddle] ${who} needs you: ${n.what}. The person pressed Ask Claude in the needs-you band: ` +
    'look into it (peek it, read its notices) and handle it, or tell them what you need from them.'
  )
}
