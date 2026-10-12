// The instant /saddle commands (#169): how each parses its arguments, the
// saddle command it runs and the short reply it shows, and that /saddle is
// registered to run at once, even while a turn is in flight. Run with
// `claude plugin test plugin`.
import { describe, expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'
import type { Engine } from 'claude-code/testing'

import type { SaddleSnapshot } from '../types'
import { REPLY_LINES, USAGE, parseSaddle, replyOf } from '../hooks/commands'
import { idle } from '../hooks/snapshot'
import { loaded, memoryState } from './fixtures'

// train has t1 queued, t2 held in the queue, and t3 in a PR stack only.
const train: SaddleSnapshot = {
  ...idle(),
  engine: 'engine',
  isLive: true,
  queue: [
    { position: 1, task: 't1', state: 'queued', note: '', seq: 1 },
    { position: 2, task: 't2', state: 'on_hold', note: 'review', seq: 2 },
  ],
  stack: {
    enabled: true,
    source: 'runtime',
    stacks: [{ id: 't3', nodes: [{ task: 't3', pr: '#9', base: 'main' }], next: '#9', ready: false }],
  },
}

describe('parseSaddle', () => {
  const runs: [string, string[]][] = [
    ['status', ['status']],
    ['  status  ', ['status']],
    ['land', ['land']],
    ['restack', ['restack']],
    ['automerge on', ['automerge', 'on']],
    ['automerge off', ['automerge', 'off']],
    ['automerge status', ['automerge', 'status']],
    ['automerge', ['automerge', 'status']],
    ['hold t1', ['queue', 'hold', 't1', 'held with /saddle hold']],
    ['hold t1 waiting on review', ['queue', 'hold', 't1', 'waiting on review']],
    ['release t2', ['queue', 'release', 't2']],
    ['hold t3', ['automerge', 'hold', 't3']],
    ['release t3', ['automerge', 'release', 't3']],
    // Neither queued nor stacked: the queue is the train's, and saddle says why.
    ['hold t9', ['queue', 'hold', 't9', 'held with /saddle hold']],
  ]
  for (const [args, argv] of runs) {
    test(`/saddle ${args.trim()} runs saddle ${argv.join(' ')}`, () => {
      const p = parseSaddle(args, train)
      expect('argv' in p ? p.argv : p.usage).toEqual(argv)
    })
  }

  test('a task with no snapshot holds in the queue', () => {
    const p = parseSaddle('release t3', null)
    expect('argv' in p ? p.argv : p.usage).toEqual(['queue', 'release', 't3'])
  })

  test('land gets a long timeout, the rest a short one', () => {
    const land = parseSaddle('land', train)
    const status = parseSaddle('status', train)
    expect('argv' in land && 'argv' in status && land.timeoutMs > status.timeoutMs).toBe(true)
  })

  const bad: [string, RegExp][] = [
    ['', /^Usage: \/saddle/],
    ['help', /^Usage: \/saddle/],
    ['launch', /^Unknown \/saddle command "launch"/],
    ['hold', /^\/saddle hold needs a task/],
    ['release', /^\/saddle release needs a task/],
    ['release t1 t2', /^\/saddle release takes one task/],
    ['automerge maybe', /^\/saddle automerge takes on, off or status/],
    ['status now', /^\/saddle status takes no arguments/],
    ['land now', /^\/saddle land takes no arguments/],
    ['restack --abort', /^\/saddle restack takes no arguments/],
  ]
  for (const [args, want] of bad) {
    test(`/saddle ${args || '(bare)'} shows usage and runs nothing`, () => {
      const p = parseSaddle(args, train)
      expect('usage' in p ? p.usage : p.argv.join(' ')).toMatch(want)
    })
  }

  test('the usage names every command', () => {
    for (const c of ['status', 'land', 'hold <task>', 'release <task>', 'restack', 'automerge on|off|status']) {
      expect(USAGE).toContain(c)
    }
  })
})

describe('replyOf', () => {
  test('shows the output, trimmed', () => {
    expect(replyOf(['land'], { text: '\nlanded t1\n\n' })).toBe('landed t1')
  })

  test('says it is done when saddle printed nothing', () => {
    expect(replyOf(['queue', 'release', 't2'], { text: '  \n' })).toBe('saddle queue release t2: done')
  })

  test('shows what failed', () => {
    expect(replyOf(['land'], { error: 'saddle land: exit 1: train busy' })).toBe('saddle land: exit 1: train busy')
  })

  test('keeps a long output short', () => {
    const lines = Array.from({ length: REPLY_LINES + 5 }, (_, i) => `line ${i}`)
    const got = replyOf(['status'], { text: lines.join('\n') }).split('\n')
    expect(got).toHaveLength(REPLY_LINES + 1)
    expect(got[REPLY_LINES]).toBe('… 5 more lines (run saddle status in a shell)')
  })
})

type Ran = { argv: string[]; cwd?: string; timeoutMs?: number }

// started registers the mod's commands in a repo saddle manages and records
// each saddle command run, answering with `reply`.
async function started($: Engine, on: On, reply: (argv: string[]) => { exitCode: number; stdout: string; stderr: string }) {
  memoryState(on, { 'saddle/snapshot': train })
  const registered: { name: string; immediate?: true; argumentHint?: string }[] = []
  const ran: Ran[] = []
  on('session.start', ($, e) => ({ cwd: e.cwd }))
  on('session.version', () => ({ value: { version: '2.1.287', base: '2.1.287' } }))
  on('session.cwd', () => ({ value: '/repo' }))
  on('fs.exists', () => ({ value: true }))
  on('command.register', ($, e) => {
    registered.push(e)
    return { value: { command: e.name } }
  })
  on('process.run', ($, e) => {
    const argv = [...e.argv]
    // The refresh timer's reads are not the command's.
    if (!argv.includes('--json')) {
      ran.push({ argv, cwd: e.init?.cwd, timeoutMs: e.init?.timeoutMs })
    }
    return { value: { ...reply(argv), isStdoutTruncated: false, isStderrTruncated: false } }
  })
  mock.env(on, {})
  mock.clock(on, { now: 1000 })
  await $.session.start({ cwd: '/repo', surface: 'terminal', isInteractive: true })
  return { registered, ran }
}

const presentation = { isFullscreen: true, columns: 200 }
function run(args: string) {
  return { command: 'saddle', args, origin: { kind: 'composer' }, presentation } as const
}

describe('/saddle', () => {
  test('is registered to run at once, without waiting for the turn', loaded, async ($, on) => {
    const { registered } = await started($, on, () => ({ exitCode: 0, stdout: '', stderr: '' }))
    const saddle = registered.find(c => c.name === 'saddle')
    expect(saddle?.immediate).toBe(true)
    expect(saddle?.argumentHint).toBeDefined()
  })

  test('runs each command through the saddle CLI and shows its reply', loaded, async ($, on) => {
    const { ran } = await started($, on, argv => ({ exitCode: 0, stdout: `${argv.slice(1).join(' ')}: ok\n`, stderr: '' }))
    const replies: (string | undefined)[] = []
    for (const args of ['status', 'land', 'hold t1', 'release t3', 'restack', 'automerge off']) {
      replies.push((await $.command.run(run(args))).text)
    }
    expect(ran.map(r => r.argv)).toEqual([
      ['saddle', 'status'],
      ['saddle', 'land'],
      ['saddle', 'queue', 'hold', 't1', 'held with /saddle hold'],
      ['saddle', 'automerge', 'release', 't3'],
      ['saddle', 'restack'],
      ['saddle', 'automerge', 'off'],
    ])
    expect(ran.map(r => r.cwd)).toEqual(Array(6).fill('/repo'))
    expect(replies[0]).toBe('status: ok')
    expect(replies[5]).toBe('automerge off: ok')
  })

  test('shows usage and runs nothing for a bad command', loaded, async ($, on) => {
    const { ran } = await started($, on, () => ({ exitCode: 0, stdout: '', stderr: '' }))
    expect((await $.command.run(run('hold'))).text).toMatch(/^\/saddle hold needs a task/)
    expect(ran).toEqual([])
  })

  test('says what failed', loaded, async ($, on) => {
    await started($, on, () => ({ exitCode: 1, stdout: '', stderr: 'error: t1 is not queued\n' }))
    expect((await $.command.run(run('release t1'))).text).toBe('saddle queue: exit 1: error: t1 is not queued')
  })
})
