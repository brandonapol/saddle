// The saddle mod's snapshot (#166): parsing each command's JSON, change
// detection, and the refresh timer, with saddle and the session stood in by
// the test's own hooks. Run with `claude plugin test plugin`.
import { describe, expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'

import type { SaddleSnapshot } from '../types'
import { idle, parseQueue, parseStack, parseStatus, sameSnapshot, versionAtLeast } from '../hooks/snapshot'

describe('versionAtLeast', () => {
  test('compares releases numerically', () => {
    expect(versionAtLeast('2.1.287', '2.1.287')).toBe(true)
    expect(versionAtLeast('2.1.300-dev', '2.1.287')).toBe(true)
    expect(versionAtLeast('2.2.0', '2.1.287')).toBe(true)
    expect(versionAtLeast('2.1.284', '2.1.287')).toBe(false)
    expect(versionAtLeast('1.9.999', '2.1.287')).toBe(false)
    expect(versionAtLeast(undefined, '2.1.287')).toBe(false)
    expect(versionAtLeast('nightly', '2.1.287')).toBe(false)
  })
})

describe('parse', () => {
  test('queue', () => {
    expect(parseQueue('{"engine":"engine","entries":[{"position":1,"task":"t1","state":"queued","seq":3}]}')).toEqual({
      value: { engine: 'engine', entries: [{ position: 1, task: 't1', state: 'queued', seq: 3 }] },
    })
    expect('error' in parseQueue('nope')).toBe(true)
    expect('error' in parseQueue('{"entries":[]}')).toBe(true)
  })
  test('status', () => {
    const got = parseStatus('{"integration":"saddle/integration","adapters":[],"tasks":[{"id":"t1","title":"x","status":"running"}]}')
    expect('value' in got && got.value.tasks[0]?.id).toBe('t1')
    expect('error' in parseStatus('{"integration":"x"}')).toBe(true)
  })
  test('stack', () => {
    expect('value' in parseStack('{"enabled":true,"source":"config","checked":"2026-10-09T00:00:00Z"}')).toBe(true)
    expect('error' in parseStack('[]')).toBe(true)
  })
})

describe('sameSnapshot', () => {
  const base: SaddleSnapshot = {
    ...idle(),
    engine: 'engine',
    isLive: true,
    stack: { enabled: true, source: 'config', checked: 'a', next_check: 'b' },
  }
  test('ignores the stamps every read moves', () => {
    expect(sameSnapshot(base, { ...base, changedAt: 9, stack: { enabled: true, source: 'config', checked: 'c', next_check: 'd' } })).toBe(true)
  })
  test('sees a real change', () => {
    expect(sameSnapshot(base, { ...base, queue: [{ position: 1, task: 't1', state: 'queued', seq: 1 }] })).toBe(false)
    expect(sameSnapshot(null, base)).toBe(false)
  })
})

type World = {
  engine: string
  // writes is every snapshot the plugin wrote, in order.
  writes: SaddleSnapshot[]
  tasks: string[]
  calls: string[]
  hasSaddleDir?: boolean
  version?: string
  interactive?: boolean
}

// world stands saddle and the session up beneath the plugin, and keeps
// $.state in memory so the test sees each write.
function world(on: On, w: World) {
  on('session.start', ($, e) => ({ cwd: e.cwd }))
  on('session.version', () => ({ value: { version: w.version ?? '2.1.287', base: w.version ?? '2.1.287' } }))
  on('fs.exists', ($, e) => ({ value: (w.hasSaddleDir ?? true) && e.path === '/repo/.saddle' }))
  on('state.get', () => ({ value: { value: w.writes.at(-1), version: w.writes.length } }))
  on('state.set', ($, e) => {
    w.writes.push(e.value as SaddleSnapshot)
    return { value: { isSet: true, version: w.writes.length } }
  })
  on('process.run', ($, e) => {
    const cmd = e.argv.slice(1).join(' ')
    w.calls.push(cmd)
    const ok = (stdout: string) => ({ value: { exitCode: 0, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } })
    switch (cmd) {
      case 'queue --json':
        return ok(JSON.stringify({ engine: w.engine, entries: [] }))
      case 'status --json':
        return ok(JSON.stringify({ integration: 'saddle/integration', tasks: w.tasks.map(id => ({ id, title: id, status: 'running' })) }))
      case 'stack --json':
        return ok(JSON.stringify({ enabled: true, source: 'config', checked: String(Math.random()) }))
      default:
        return { value: { exitCode: 1, stdout: '', stderr: `unknown ${cmd}`, isStdoutTruncated: false, isStderrTruncated: false } }
    }
  })
  on('command.register', ($, e) => ({ value: { command: e.name } }))
  mock.env(on, {})
  return mock.clock(on, { now: 1000 })
}

const start = { cwd: '/repo', surface: 'terminal', isInteractive: true } as const

describe('refresh', () => {
  test('reads saddle at start and writes the snapshot', async ($, on) => {
    const w: World = { engine: 'engine', tasks: ['t1'], calls: [], writes: [] }
    const clock = world(on, w)
    await $.session.start(start)
    await clock.settle()
    const value = w.writes.at(-1)
    expect(value?.isLive).toBe(true)
    expect(value?.engine).toBe('engine')
    expect(value?.status?.tasks.map(t => t.id)).toEqual(['t1'])
    expect(value?.stack?.enabled).toBe(true)
    expect(value?.errors).toEqual([])
    expect(w.calls).toEqual(['queue --json', 'status --json', 'stack --json'])
  })

  test('writes only on a change, and reads the stack less often', async ($, on) => {
    const w: World = { engine: 'engine', tasks: ['t1'], calls: [], writes: [] }
    const clock = world(on, w)
    await $.session.start(start)
    await clock.settle()
    expect(w.writes).toHaveLength(1)

    await clock.advance(3000)
    expect(w.calls.filter(c => c === 'status --json')).toHaveLength(2)
    expect(w.writes).toHaveLength(1)
    expect(w.calls.filter(c => c === 'stack --json')).toHaveLength(1)

    w.tasks = ['t1', 't2']
    await clock.advance(3000)
    expect(w.writes).toHaveLength(2)
    expect(w.writes[1]?.status?.tasks.map(t => t.id)).toEqual(['t1', 't2'])
    expect(w.writes[1]?.changedAt).toBe(7000)

    await clock.advance(30000)
    expect(w.calls.filter(c => c === 'stack --json').length).toBeGreaterThan(1)
  })

  test('goes idle when the engine stops, without reading status', async ($, on) => {
    const w: World = { engine: 'engine', tasks: ['t1'], calls: [], writes: [] }
    const clock = world(on, w)
    await $.session.start(start)
    await clock.settle()
    w.engine = ''
    w.calls = []
    await clock.advance(3000)
    const value = w.writes.at(-1)
    expect(value?.isLive).toBe(false)
    expect(value?.status).toBeNull()
    expect(w.calls).toEqual(['queue --json'])
  })

  test('does nothing outside a saddle repo', async ($, on) => {
    const w: World = { engine: 'engine', tasks: [], calls: [], writes: [], hasSaddleDir: false }
    const clock = world(on, w)
    await $.session.start(start)
    await clock.advance(10000)
    expect(w.calls).toEqual([])
    expect(w.writes).toEqual([])
  })

  test('does nothing where nothing is drawn', async ($, on) => {
    const w: World = { engine: 'engine', tasks: [], calls: [], writes: [] }
    const clock = world(on, w)
    await $.session.start({ cwd: '/repo', surface: null, isInteractive: false })
    await clock.advance(10000)
    expect(w.calls).toEqual([])
    expect(w.writes).toEqual([])
  })

  test('does nothing on a Claude Code older than the mods API', async ($, on) => {
    const w: World = { engine: 'engine', tasks: [], calls: [], writes: [], version: '2.1.284' }
    const clock = world(on, w)
    await $.session.start(start)
    await clock.advance(10000)
    expect(w.calls).toEqual([])
    expect(w.writes).toEqual([])
  })
})
