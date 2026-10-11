// Waking the orchestrator (#170): which events submit a `[saddle]` prompt
// and which only toast, the heartbeat that keeps the settings hook and
// `saddle plugin wait` off, and the routine backlog riding on the person's
// next message. Run with `claude plugin test plugin`.
import { describe, expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'

import type { SaddleSnapshot } from '../types'
import { idle } from '../hooks/snapshot'
import { HEARTBEAT, backlogContext, hasEvents, kept, parseEvents, planWake, wakePrompt, wakes } from '../hooks/wake'
import type { SaddleEvent } from '../hooks/wake'
import { memoryState } from './fixtures'

const landed: SaddleEvent = { kind: 'info', text: 't3 (meter) landed on saddle/integration.' }
const finished: SaddleEvent = { kind: 'info', text: 't4 (claims) finished: done.' }
const stuck: SaddleEvent = { kind: 'action', text: 't5 (spawn) is waiting on a prompt.\nDo you want to proceed?' }
const conflicted: SaddleEvent = { kind: 'action', text: 't6 (train) conflicted with saddle/integration.' }

function live(pending: number, engine = 'engine'): SaddleSnapshot {
  return {
    ...idle(),
    engine,
    isLive: true,
    status: { integration: 'saddle/integration', tasks: [{ id: 't0', title: 'orchestrator', status: 'idle', pending_notices: pending }] },
  }
}

describe('which events wake Claude', () => {
  test('only action events wake; info events do not', () => {
    expect(wakes(stuck)).toBe(true)
    expect(wakes(conflicted)).toBe(true)
    expect(wakes(landed)).toBe(false)
    expect(wakes(finished)).toBe(false)
    expect(wakes({ kind: 'something new', text: 'x' })).toBe(false)
  })

  test('asks for events only while the plugin engine runs and t0 has some', () => {
    expect(hasEvents(null)).toBe(false)
    expect(hasEvents(idle())).toBe(false)
    expect(hasEvents(live(0))).toBe(false)
    expect(hasEvents(live(2))).toBe(true)
    // Under saddle up the TUI orchestrator owns t0's events.
    expect(hasEvents(live(2, 'up'))).toBe(false)
  })

  test('the wake prompt leads with what needs a decision, then the backlog', () => {
    const text = wakePrompt([stuck, conflicted], kept([], [landed]))
    expect(text).toBe(
      '[saddle] t5 (spawn) is waiting on a prompt.\nDo you want to proceed?\n\n' +
        '[saddle] t6 (train) conflicted with saddle/integration.\n\n' +
        'Also since your last update:\n\n[saddle] t3 (meter) landed on saddle/integration.',
    )
  })

  test('the backlog keeps the newest twenty', () => {
    const many = Array.from({ length: 25 }, (_, i) => ({ kind: 'info', text: `e${i}` }))
    const got = kept([], many)
    expect(got).toHaveLength(20)
    expect(got[0]).toBe('[saddle] e5')
  })

  test('plans a batch: routine events toast and wait, a decision wakes with them', () => {
    const quiet = planWake([landed], [])
    expect(quiet).toEqual({
      toasts: ['[saddle] t3 (meter) landed on saddle/integration.'],
      prompt: null,
      backlog: ['[saddle] t3 (meter) landed on saddle/integration.'],
    })
    const loud = planWake([finished, stuck], quiet.backlog)
    expect(loud.toasts).toEqual(['[saddle] t4 (claims) finished: done.'])
    expect(loud.prompt).toBe(wakePrompt([stuck], kept(quiet.backlog, [finished])))
    expect(loud.backlog).toEqual([])
    // A multi-line event toasts its first line only.
    expect(planWake([{ kind: 'info', text: 'one\ntwo' }], []).toasts).toEqual(['[saddle] one'])
    expect(planWake([], [])).toEqual({ toasts: [], prompt: null, backlog: [] })
  })

  test("the person's next message carries the backlog, if any", () => {
    expect(backlogContext([])).toBeNull()
    expect(backlogContext(['[saddle] a', '[saddle] b'])).toBe('[saddle] a\n\n[saddle] b')
  })

  test('parses saddle plugin events --json', () => {
    expect(parseEvents('{"events":[{"kind":"action","text":"a"},{"kind":"info","text":"b"}]}')).toEqual({
      value: [
        { kind: 'action', text: 'a' },
        { kind: 'info', text: 'b' },
      ],
    })
    expect(parseEvents('{"events":[]}')).toEqual({ value: [] })
    expect('error' in parseEvents('not json')).toBe(true)
    expect('error' in parseEvents('{}')).toBe(true)
  })
})

type World = {
  engine: string
  /** The events saddle holds for t0; `plugin events` takes them. */
  events: SaddleEvent[]
  calls: string[]
  beats: string[]
  prompts: string[]
  toasts: string[]
}

// world stands saddle and the session up beneath the plugin: status counts
// t0's pending events, and `saddle plugin events --json` takes them.
function world(on: On, w: World, env: Record<string, string> = {}) {
  memoryState(on)
  on('session.start', ($, e) => ({ cwd: e.cwd }))
  on('session.version', () => ({ value: { version: '2.1.287', base: '2.1.287' } }))
  on('session.cwd', () => ({ value: '/repo' }))
  on('fs.exists', ($, e) => ({ value: e.path === '/repo/.saddle' }))
  on('fs.write', ($, e) => {
    w.beats.push(e.path)
    return { value: undefined }
  })
  on('process.run', ($, e) => {
    const cmd = e.argv.slice(1).join(' ')
    w.calls.push(cmd)
    const ok = (stdout: string) => ({ value: { exitCode: 0, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } })
    switch (cmd) {
      case 'queue --json':
        return ok(JSON.stringify({ engine: w.engine, entries: [] }))
      case 'status --json':
        return ok(
          JSON.stringify({
            integration: 'saddle/integration',
            tasks: [{ id: 't0', title: 'orchestrator', status: 'idle', pending_notices: w.events.length }],
          }),
        )
      case 'stack --json':
        return ok(JSON.stringify({ enabled: false, source: 'config' }))
      case 'plugin events --json': {
        const events = w.engine === 'engine' ? w.events : []
        w.events = w.engine === 'engine' ? [] : w.events
        return ok(JSON.stringify({ events }))
      }
      default:
        return { value: { exitCode: 1, stdout: '', stderr: `unknown ${cmd}`, isStdoutTruncated: false, isStderrTruncated: false } }
    }
  })
  on('command.register', ($, e) => ({ value: { command: e.name } }))
  on('prompt.submit', ($, e) => {
    w.prompts.push(e.text)
    if (e.context?.length) {
      w.prompts.push(`context: ${e.context.join(' | ')}`)
    }
    return { text: e.text }
  })
  on('ui.toast', ($, e) => {
    w.toasts.push(e.text)
    return { value: undefined }
  })
  mock.env(on, env)
  return mock.clock(on, { now: 1000 })
}

const start = { cwd: '/repo', surface: 'terminal', isInteractive: true } as const

function fresh(over: Partial<World> = {}): World {
  return { engine: 'engine', events: [], calls: [], beats: [], prompts: [], toasts: [], ...over }
}

describe('wake', () => {
  test('writes the heartbeat each tick, and runs no extra command while nothing waits', async ($, on) => {
    const w = fresh()
    const clock = world(on, w)
    await $.session.start(start)
    await clock.settle()
    await clock.advance(6000)
    expect(w.beats.length).toBeGreaterThanOrEqual(3)
    expect(new Set(w.beats)).toEqual(new Set([`/repo/${HEARTBEAT}`]))
    expect(w.calls).not.toContain('plugin events --json')
    expect(w.prompts).toEqual([])
  })

  test('an event that needs a decision submits one [saddle] prompt', async ($, on) => {
    const w = fresh()
    const clock = world(on, w)
    await $.session.start(start)
    await clock.settle()
    w.events = [stuck, conflicted]
    await clock.advance(6000)
    expect(w.prompts).toEqual([
      '[saddle] t5 (spawn) is waiting on a prompt.\nDo you want to proceed?\n\n[saddle] t6 (train) conflicted with saddle/integration.',
    ])
    // Taken once: a later tick submits nothing more.
    await clock.advance(6000)
    expect(w.prompts).toHaveLength(1)
    expect(w.calls.filter(c => c === 'plugin events --json')).toHaveLength(1)
  })

  test('routine events only toast, then ride along on the next wake', async ($, on) => {
    const w = fresh()
    const clock = world(on, w)
    await $.session.start(start)
    await clock.settle()
    w.events = [landed, finished]
    await clock.advance(6000)
    expect(w.prompts).toEqual([])
    expect(w.toasts).toEqual(['[saddle] t3 (meter) landed on saddle/integration.', '[saddle] t4 (claims) finished: done.'])

    w.events = [conflicted]
    await clock.advance(6000)
    expect(w.prompts).toEqual([
      '[saddle] t6 (train) conflicted with saddle/integration.\n\nAlso since your last update:\n\n' +
        '[saddle] t3 (meter) landed on saddle/integration.\n\n[saddle] t4 (claims) finished: done.',
    ])
  })

  test("routine events ride on the person's next message instead", async ($, on) => {
    const w = fresh()
    const clock = world(on, w)
    await $.session.start(start)
    await clock.settle()
    w.events = [landed]
    await clock.advance(6000)
    w.prompts = []
    await $.prompt.submit({ text: 'what landed?' })
    expect(w.prompts).toEqual(['what landed?', 'context: [saddle] t3 (meter) landed on saddle/integration.'])
    // Delivered once.
    w.prompts = []
    await $.prompt.submit({ text: 'and now?' })
    expect(w.prompts).toEqual(['and now?'])
  })

  test("leaves events alone under saddle up: they are the TUI orchestrator's", async ($, on) => {
    const w = fresh({ engine: 'up', events: [stuck] })
    const clock = world(on, w)
    await $.session.start(start)
    await clock.advance(9000)
    expect(w.calls).not.toContain('plugin events --json')
    expect(w.prompts).toEqual([])
  })

  test('does nothing in a worker session', async ($, on) => {
    const w = fresh({ events: [stuck] })
    const clock = world(on, w, { SADDLE_TASK: 't9' })
    await $.session.start(start)
    await clock.advance(9000)
    expect(w.beats).toEqual([])
    expect(w.prompts).toEqual([])
  })
})
