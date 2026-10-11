// The saddle mod's state contract (#166): one snapshot of saddle's state,
// refreshed on a timer by hooks/register.ts, that the pane, the needs-you
// band and the commands read with `read($, snapshot)`.

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
  pr?: string
  window?: string
  worktree?: string
}

/** `saddle status --json`, the fields the mod reads; the rest pass through. */
export type SaddleStatus = {
  integration: string
  warnings?: string[]
  tasks: SaddleTask[]
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

/** `saddle stack --json`: the PR stacks and the auto-merge watcher. */
export type SaddleStack = {
  enabled: boolean
  source: string
  stopped?: string
  holds?: string[]
  stacks?: unknown[]
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

declare module 'claude-code' {
  interface PluginState {
    saddle: { snapshot: SaddleSnapshot | null }
  }
}
