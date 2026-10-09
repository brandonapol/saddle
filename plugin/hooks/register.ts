// The saddle mod's hooks module (#165). This scaffold keeps the shared
// snapshot fresh; the pane, band and commands build on it. A timer runs
// `saddle queue --json` (cheap: it reads the store and says whether the
// engine runs), then while saddle runs `saddle status --json`, and `saddle
// stack --json` less often since that one asks GitHub. The snapshot is
// written to $.state only when it changed, and that write is what redraws
// every hook that read it.
//
// It does nothing where nothing is drawn (`claude -p`, the SDK), on a Claude
// Code older than the mods API it needs, in saddle's own worker sessions, or
// outside a repo saddle manages. The settings hooks in hooks.json keep
// working either way.
import { atom, read } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { SaddleSnapshot } from '../types'
import {
  MIN_VERSION,
  POLL_MS,
  STACK_MS,
  idle,
  parseQueue,
  parseStack,
  parseStatus,
  sameSnapshot,
  versionAtLeast,
} from './snapshot'

// The shared snapshot. Another hook in this module reads it with
// `read($, snapshot)`, which subscribes its drawing to every change.
const snapshotRef = { plugin: 'saddle', key: 'snapshot' } as const
const snapshot = atom(snapshotRef, null)

type Ran = { text: string } | { error: string }

async function saddle($: EngineInterface, cwd: string, args: string[], timeoutMs: number): Promise<Ran> {
  try {
    const r = await $.process.run(['saddle', ...args], { cwd, timeoutMs })
    if (r.exitCode !== 0) {
      const why = r.stderr.trim().split('\n')[0] ?? ''
      return { error: `saddle ${args[0]}: exit ${r.exitCode}${why ? `: ${why}` : ''}` }
    }
    return { text: r.stdout }
  } catch (err) {
    return { error: `saddle ${args[0]}: ${err instanceof Error ? err.message : String(err)}` }
  }
}

// One poller per load: a tick that starts while one still runs does
// nothing. Module variables are fine here: a reload starts a fresh poller,
// and the snapshot itself lives in $.state.
let isBusy = false
let stackAt = -Infinity

async function refresh($: EngineInterface, cwd: string) {
  if (isBusy) {
    return
  }
  isBusy = true
  try {
    const prev = (await read($, snapshot)) ?? null
    const next: SaddleSnapshot = { ...(prev ?? idle()), errors: [] }
    const q = await saddle($, cwd, ['queue', '--json'], 10000)
    const queue = 'text' in q ? parseQueue(q.text) : q
    if ('error' in queue) {
      next.errors.push(queue.error)
    } else if (queue.value.engine === '') {
      Object.assign(next, idle())
    } else {
      const wasLive = next.isLive
      next.engine = queue.value.engine
      next.isLive = true
      next.queue = queue.value.entries
      const s = await saddle($, cwd, ['status', '--json'], 10000)
      const status = 'text' in s ? parseStatus(s.text) : s
      if ('error' in status) {
        next.errors.push(status.error)
      } else {
        next.status = status.value
      }
      const now = await $.clock.now()
      if (!wasLive || now - stackAt >= STACK_MS) {
        stackAt = now
        const k = await saddle($, cwd, ['stack', '--json'], 30000)
        const stack = 'text' in k ? parseStack(k.text) : k
        if ('error' in stack) {
          next.errors.push(stack.error)
        } else {
          next.stack = stack.value
        }
      }
    }
    if (!sameSnapshot(prev, next)) {
      next.changedAt = await $.clock.now()
      await $.state.set(snapshotRef, next)
    }
  } finally {
    isBusy = false
  }
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    const started = await next(e)
    if (!e.isInteractive || e.surface === null) {
      return started
    }
    const { version, base } = await $.session.version()
    if (!versionAtLeast(base ?? version, MIN_VERSION)) {
      return started
    }
    if (await $.env.get('SADDLE_TASK')) {
      return started
    }
    if (!(await $.fs.exists(`${e.cwd}/.saddle`))) {
      return started
    }
    // A refresh that throws is retried by the next tick; the engine logs it.
    const tick = () => void refresh($, e.cwd).catch(() => undefined)
    $.clock.every(POLL_MS, tick)
    tick()

    return started
  })
}
