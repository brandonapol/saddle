// The saddle snapshot (#166): what the mod's pane, band and commands draw
// from, and the pure parts of keeping it: parsing each command's JSON and
// telling whether a reading changed. register.ts runs the commands; the
// engine only follows $ within one file, so nothing here takes it.
import type { SaddleQueueEntry, SaddleSnapshot, SaddleStack, SaddleStatus } from '../types'

/** The oldest Claude Code that has the mods API this module uses. */
export const MIN_VERSION = '2.1.287'
/** How often the queue (and, while saddle runs, status) is read. */
export const POLL_MS = 3000
/** How often the stack is read while saddle runs: it calls GitHub. */
export const STACK_MS = 30000

/** True when `version`'s release is `min` or newer; false when unreadable. */
export function versionAtLeast(version: string | undefined, min: string): boolean {
  const parse = (v: string) => v.match(/^(\d+)\.(\d+)\.(\d+)/)?.slice(1).map(Number)
  const have = version ? parse(version) : undefined
  const want = parse(min)
  if (!have || !want) {
    return false
  }
  for (let i = 0; i < 3; i++) {
    if (have[i] !== want[i]) {
      return (have[i] ?? 0) > (want[i] ?? 0)
    }
  }
  return true
}

type Parsed<T> = { value: T } | { error: string }

function parseJSON(name: string, text: string): Parsed<unknown> {
  try {
    return { value: JSON.parse(text) }
  } catch {
    return { error: `saddle ${name} --json: not JSON` }
  }
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
}

export function parseQueue(text: string): Parsed<{ engine: string; entries: SaddleQueueEntry[] }> {
  const p = parseJSON('queue', text)
  if ('error' in p) {
    return p
  }
  const v = p.value
  if (!isObject(v) || typeof v.engine !== 'string' || !Array.isArray(v.entries)) {
    return { error: 'saddle queue --json: want {engine, entries}' }
  }
  return { value: { engine: v.engine, entries: v.entries as SaddleQueueEntry[] } }
}

export function parseStatus(text: string): Parsed<SaddleStatus> {
  const p = parseJSON('status', text)
  if ('error' in p) {
    return p
  }
  const v = p.value
  if (!isObject(v) || !Array.isArray(v.tasks)) {
    return { error: 'saddle status --json: want {tasks}' }
  }
  return { value: v as SaddleStatus }
}

export function parseStack(text: string): Parsed<SaddleStack> {
  const p = parseJSON('stack', text)
  if ('error' in p) {
    return p
  }
  const v = p.value
  if (!isObject(v) || typeof v.enabled !== 'boolean') {
    return { error: 'saddle stack --json: want {enabled}' }
  }
  return { value: v as SaddleStack }
}

/**
 * Whether two snapshots differ in anything a drawing shows: the stamps that
 * move on every read (changedAt, the stack's checked and next_check) don't
 * count, so an idle fleet redraws nothing.
 */
export function sameSnapshot(a: SaddleSnapshot | null, b: SaddleSnapshot | null): boolean {
  const key = (s: SaddleSnapshot | null) => {
    if (!s) {
      return 'null'
    }
    const stack = s.stack ? { ...s.stack, checked: undefined, next_check: undefined } : null
    return JSON.stringify({ ...s, changedAt: 0, stack })
  }
  return key(a) === key(b)
}

/** An empty snapshot: saddle isn't running. */
export function idle(): SaddleSnapshot {
  return { engine: '', isLive: false, status: null, queue: [], stack: null, errors: [], changedAt: 0 }
}
