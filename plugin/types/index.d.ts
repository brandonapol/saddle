// The saddle mod's state contract (#166): one snapshot of saddle's state,
// refreshed on a timer by hooks/register.tsx, that the pane, the needs-you
// band and the commands read with `read($, snapshot)`; and the pane's own
// view (#167): its tab and where its rows are scrolled; and the needs-you
// band's (#168): the screen it peeked, if any; and, in a worker session, the
// claim guard's holds (#171).

/** One task as `saddle status --json` lists it. */
export type SaddleTask = {
  id: string
  title: string
  status: string
  reason?: string
  model?: string
  parent?: string
  branch?: string
  claims?: string[]
  train?: string
  pending_notices?: number
  /** What it is doing now, when status reports it. */
  activity?: string
  pr?: string
  window?: string
  worktree?: string
}

/** A PR whose CI is red, as `saddle status --json` lists it under ci_red. */
export type SaddleCIRed = {
  task: string
  pr: string
  head?: string
  /** The checks that failed. */
  checks?: string[]
  /** The tasks stacked on it, held until it is green. */
  held?: string[]
  /** The task repairing it, if one is. */
  repair?: string
  attempts?: number
}

/** `saddle status --json`, the fields the mod reads; the rest pass through. */
export type SaddleStatus = {
  integration: string
  warnings?: string[]
  tasks: SaddleTask[]
  ci_red?: SaddleCIRed[]
  [field: string]: unknown
}

/** One waiting entry of `saddle queue --json`, next first. */
export type SaddleQueueEntry = {
  position: number
  task: string
  state: string
  note?: string
  seq: number
  attempts?: number
}

/** One PR of a stack, bottom first, as the auto-merge watcher read it. */
export type SaddleStackNode = {
  task: string
  branch?: string
  pr: string
  /** The branch it targets: base, or the PR below it. */
  base: string
  draft?: boolean
  /** ChecksPass, ChecksPending, ChecksFail or ChecksNone. */
  checks?: string
  /** MERGEABLE, CONFLICTING or UNKNOWN. */
  mergeable?: string
  /** CLEAN, BLOCKED, BEHIND, DIRTY, UNSTABLE, UNKNOWN... */
  merge_state?: string
  labels?: string[]
  at_risk?: string
  error?: string
}

/** One PR stack, named by its bottom task. */
export type SaddleStackGraph = {
  id: string
  nodes: SaddleStackNode[]
  held?: boolean
  /** Commits base has that the stack's top lacks. */
  behind?: number
  next: string
  ready: boolean
  why?: string
  blocked?: string
}

/** `saddle stack --json`: the PR stacks and the auto-merge watcher. */
export type SaddleStack = {
  enabled: boolean
  source: string
  stopped?: string
  busy?: boolean
  holds?: string[]
  stacks?: SaddleStackGraph[]
  checked?: string
  next_check?: string
  [field: string]: unknown
}

export type SaddleSnapshot = {
  /** Who holds saddle's engine lock: "engine", "up", or "" when nothing runs. */
  engine: string
  /** True while saddle runs; the rest is the last reading while it does. */
  isLive: boolean
  status: SaddleStatus | null
  queue: SaddleQueueEntry[]
  stack: SaddleStack | null
  /** What failed in the last refresh, one line per command. */
  errors: string[]
  /** When the snapshot last changed, ms since the epoch. */
  changedAt: number
}

/** The pane's tabs, in the order they are drawn. */
export type SaddlePaneTab = 'agents' | 'claims' | 'train' | 'stacks'

/** The pane's view: the tab shown and the first body row shown. */
export type SaddlePane = {
  tab: SaddlePaneTab
  offset: number
}

/** The needs-you band's view: the task whose screen it shows, if any. */
export type SaddleBand = {
  peek: { task: string; screen: string } | null
}

/** A call the claim guard held (#171), shown in its band until handled. */
export type SaddleGuardHold = {
  /** The repo-relative path outside the task's claims. */
  path: string
  /** The tool whose call was held. */
  tool: string
}

/**
 * The claim guard's state in a worker session: the holds the band shows, and
 * every path it has held once, which passes when the worker retries it.
 */
export type SaddleGuard = {
  pending: SaddleGuardHold[]
  held: string[]
}

declare module 'claude-code' {
  interface PluginState {
    saddle: { snapshot: SaddleSnapshot | null; pane: SaddlePane; band: SaddleBand; guard: SaddleGuard }
  }
}
