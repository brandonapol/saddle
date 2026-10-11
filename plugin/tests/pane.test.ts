// The saddle pane (#167): each tab's rows from fixture snapshots, and the
// pane itself drawn on the terminal and the desktop, its tabs pressed, its
// rows scrolled and `/saddle-pane` toggling it. Run with `claude plugin test
// plugin`.
import { describe, expect, mock, test } from 'claude-code/testing'
import type { On, RenderPropsOf } from 'claude-code'
import type { Engine } from 'claude-code/testing'

import type { SaddleSnapshot } from '../types'
import { agentLines, claimLines, headerOf, scrolled, stackLines, tabLines, trainLines } from '../hooks/pane'
import { idle } from '../hooks/snapshot'
import { fleet, memoryState } from './fixtures'

const texts = (lines: { text: string }[]) => lines.map(l => l.text)

describe('header', () => {
  test('says how the fleet stands', () => {
    expect(headerOf(null)).toBe('saddle · reading…')
    expect(headerOf(idle())).toBe('saddle · not running')
    expect(headerOf(fleet)).toBe('saddle · engine · 3 active · 1 needs you · 2 in train')
  })
})

describe('agents tab', () => {
  test('lists id, state, model and title, its activity under it, finished ones last', () => {
    expect(texts(agentLines(fleet))).toEqual([
      't2  running      opus    Saddle pane',
      '    Edit plugin/hooks/pane.ts',
      't3  needs_you    sonnet  Needs-you band',
      '    2 notices waiting',
      't4  done         opus    Claim guard',
      '    train: queued',
      't5  failed       -       Broken spawn',
      '    worktree exists',
      't1  landed       opus    Mod scaffold',
    ])
  })
  test('colors what needs a look', () => {
    const tone = (prefix: string) => agentLines(fleet).find(l => l.text.startsWith(prefix))?.tone
    expect(tone('t3')).toBe('warn')
    expect(tone('t5')).toBe('bad')
    expect(tone('t4')).toBe('good')
    expect(tone('t1')).toBe('dim')
    expect(tone('t2')).toBeUndefined()
  })
  test('says when there is nothing to show', () => {
    expect(texts(agentLines(null))).toEqual(['Reading saddle…'])
    expect(texts(agentLines(idle()))).toEqual(["saddle isn't running: start `saddle plugin engine` or `saddle up`."])
    expect(texts(agentLines({ ...fleet, status: { integration: 'x', tasks: [] } }))).toEqual(['No agents yet.'])
  })
})

describe('claims tab', () => {
  test('lists each live task with its globs', () => {
    expect(texts(claimLines(fleet))).toEqual(['t2  plugin/**', 't3  plugin/hooks/band.ts', '    docs/band.md'])
  })
  test('says when none are held', () => {
    expect(texts(claimLines({ ...fleet, status: { integration: 'x', tasks: [{ id: 't1', title: 'x', status: 'running' }] } }))).toEqual([
      'No claims held.',
    ])
  })
})

describe('train tab', () => {
  test('lists the queue next first, with its holds', () => {
    expect(texts(trainLines(fleet))).toEqual([
      '1. t4  queued',
      '2. t6  on_hold  waiting on CI  (2 attempts)',
      'Auto-merge holds: t9',
    ])
    expect(trainLines(fleet)[1]?.tone).toBe('warn')
  })
  test('says when it is empty', () => {
    expect(texts(trainLines({ ...fleet, queue: [], stack: null }))).toEqual(['The train is empty.'])
  })
})

describe('stacks tab', () => {
  test('draws each stack: base, CI, mergeability and behind-base', () => {
    expect(texts(stackLines(fleet))).toEqual([
      'Auto-merge on (runtime) · holds: t9',
      'stack t7 · 3 behind base',
      '  1. t7 #41 → main  checks pass · mergeable · clean',
      '  2. t8 #42 → saddle/t7  checks fail · conflicting · dirty · draft · at risk: red CI',
      '  next: #41 is ready to merge',
      'stack t9 · held',
      "  1. t9 #50 → main  can't read: gh: timeout",
      '  next: #50 waits: held',
    ])
    const lines = stackLines(fleet)
    expect(lines[1]?.tone).toBe('head')
    expect(lines[3]?.tone).toBe('bad')
    expect(lines[4]?.tone).toBe('good')
    expect(lines[7]?.tone).toBe('warn')
  })
  test('says when the watcher stopped or nothing is stacked', () => {
    const stopped = { ...fleet, stack: { enabled: false, source: 'config', stopped: 'merge of #41 failed' } }
    expect(texts(stackLines(stopped))).toEqual([
      'Auto-merge off (config)',
      'Stopped: merge of #41 failed. `saddle automerge on` resumes it.',
      'No open PR stacks.',
    ])
    expect(texts(stackLines({ ...fleet, stack: null }))).toEqual(['Stacks not read yet.'])
  })
})

describe('tabLines', () => {
  test('leads with what the last refresh could not read', () => {
    const lines = tabLines({ ...fleet, errors: ['saddle status: exit 1'] }, 'claims')
    expect(lines[0]).toEqual({ text: '! saddle status: exit 1', tone: 'bad' })
    expect(lines[1]?.text).toBe('t2  plugin/**')
  })
})

describe('scrolled', () => {
  test('clamps to the rows', () => {
    expect(scrolled(0, 3, 10, 4)).toBe(3)
    expect(scrolled(5, 9, 10, 4)).toBe(6)
    expect(scrolled(2, -9, 10, 4)).toBe(0)
    expect(scrolled(0, 3, 3, 4)).toBe(0)
  })
})

const PANE = 'saddle'

function props(bodyRows = 30): RenderPropsOf['Pane'] {
  return {
    title: 'saddle',
    isFocused: true,
    bodyColumns: 80,
    placement: 'dock',
    scroll: { offset: 0, bodyRows },
    view: {},
  }
}

// mountPane draws the pane over `snap`, held in $.state.
//
// The test's $.state stands in beneath the plugin (memoryState), so a write
// doesn't redraw its readers as the host's does: the tests redraw after
// each act that writes, as the host would.
async function mountPane($: Engine, on: On, surface: 'terminal' | 'desktop', snap: SaddleSnapshot | null, bodyRows?: number) {
  memoryState(on, { 'saddle/snapshot': snap })
  const ui = await $.ui.mount({ plugin: 'saddle', surface, component: 'Pane', requestId: PANE, props: props(bodyRows) })
  return {
    ...ui,
    press: async (target: { key: string }) => {
      await ui.press(target)
      await ui.redraw()
    },
  }
}

// scroll is the person's scroll keys moving the pane's window by `by` rows.
function scroll(by: number) {
  return {
    component: 'Pane',
    requestId: PANE,
    offset: by,
    by,
    bodyRows: 5,
    contentRows: 5,
    origin: { kind: 'person' },
  } as const
}

describe('pane', () => {
  for (const surface of ['terminal', 'desktop'] as const) {
    test(`draws the agents tab first and switches tabs (${surface})`, async ($, on) => {
      const ui = await mountPane($, on, surface, fleet)
      expect((await ui.find({ key: 'header' }))?.text).toBe('saddle · engine · 3 active · 1 needs you · 2 in train')
      expect(await ui.find({ type: 'Text', text: /^t2 {2}running/ })).toBeDefined()
      expect((await ui.find({ key: 'tab-agents' }))?.props.variant).toBe('primary')

      await ui.press({ key: 'tab-claims' })
      expect((await ui.find({ key: 'tab-claims' }))?.props.variant).toBe('primary')
      expect(await ui.find({ type: 'Text', text: 't3  plugin/hooks/band.ts' })).toBeDefined()
      expect(await ui.find({ type: 'Text', text: /^t2 {2}running/ })).toBeUndefined()

      await ui.press({ key: 'tab-train' })
      expect(await ui.find({ type: 'Text', text: '1. t4  queued' })).toBeDefined()

      await ui.press({ key: 'tab-stacks' })
      expect(await ui.find({ type: 'Text', text: 'stack t7 · 3 behind base' })).toBeDefined()
      await ui.unmount()
    })
  }

  test('draws what each refresh writes', async ($, on) => {
    memoryState(on, { 'saddle/snapshot': null })
    let tasks = [{ id: 't1', title: 'one', status: 'running' }]
    const ok = (stdout: string) => ({ value: { exitCode: 0, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } })
    on('session.start', ($, e) => ({ cwd: e.cwd }))
    on('session.version', () => ({ value: { version: '2.1.287', base: '2.1.287' } }))
    on('fs.exists', () => ({ value: true }))
    on('command.register', ($, e) => ({ value: { command: e.name } }))
    on('process.run', ($, e) => {
      switch (e.argv[1]) {
        case 'queue':
          return ok(JSON.stringify({ engine: 'engine', entries: [] }))
        case 'status':
          return ok(JSON.stringify({ integration: 'saddle/integration', tasks }))
        default:
          return ok(JSON.stringify({ enabled: false, source: 'config' }))
      }
    })
    mock.env(on, {})
    const clock = mock.clock(on, { now: 1000 })
    const ui = await $.ui.mount({ plugin: 'saddle', surface: 'terminal', component: 'Pane', requestId: PANE, props: props() })
    expect(await ui.find({ type: 'Text', text: 'Reading saddle…' })).toBeDefined()

    await $.session.start({ cwd: '/repo', surface: 'terminal', isInteractive: true })
    await clock.settle()
    await ui.redraw()
    expect(await ui.find({ type: 'Text', text: /^t1 {2}running/ })).toBeDefined()

    tasks = [...tasks, { id: 't2', title: 'two', status: 'needs_you' }]
    await clock.advance(3000)
    await ui.redraw()
    expect(await ui.find({ type: 'Text', text: /^t2 {2}needs_you/ })).toBeDefined()
    expect((await ui.find({ key: 'header' }))?.text).toContain('1 needs you')
    await ui.unmount()
  })

  test('keeps the header and tabs while its rows scroll', async ($, on) => {
    // 2 rows of header and tabs, 3 of body.
    const ui = await mountPane($, on, 'terminal', fleet, 5)
    expect(await ui.find({ type: 'Text', text: /^t2 {2}running/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /^t5/ })).toBeUndefined()

    await $.ui.scroll(scroll(6))
    await ui.redraw()
    expect(await ui.find({ key: 'header' })).toBeDefined()
    expect(await ui.find({ key: 'tab-agents' })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /^t2 {2}running/ })).toBeUndefined()
    expect(await ui.find({ type: 'Text', text: /^t5/ })).toBeDefined()

    // A new tab starts at its top.
    await ui.press({ key: 'tab-claims' })
    expect(await ui.find({ type: 'Text', text: 't2  plugin/**' })).toBeDefined()
    await ui.unmount()
  })
})

type Panes = { open: string[]; opened: { id: string; focus?: true; closeOnEscape?: true }[] }

// panes stands the engine's pane record up beneath the plugin.
function panes(on: On, p: Panes) {
  on('ui.open', ($, e) => {
    p.opened.push(e)
    if (!p.open.includes(e.id)) {
      p.open.push(e.id)
    }
    return { value: { isPlaced: true } }
  })
  on('ui.close', ($, e) => {
    p.open = p.open.filter(id => id !== e.id)
    return { value: undefined }
  })
  on('ui.panes', () => ({ value: p.open.map(id => ({ id, title: 'saddle', isShown: true, isFocused: false, isPlaced: true })) }))
}

// run is the person typing /saddle-pane.
function run(presentation: { isFullscreen: boolean; columns: number }) {
  return { command: 'saddle-pane', args: '', origin: { kind: 'composer' }, presentation } as const
}

describe('/saddle-pane', () => {
  test('opens the pane, then closes it', async ($, on) => {
    const p: Panes = { open: [], opened: [] }
    panes(on, p)
    const fullscreen = run({ isFullscreen: true, columns: 200 })
    expect((await $.command.run(fullscreen)).text).toBe('Saddle pane shown')
    expect(p.open).toEqual([PANE])
    expect(p.opened[0]?.focus).toBeUndefined()
    expect((await $.command.run(fullscreen)).text).toBe('Saddle pane hidden')
    expect(p.open).toEqual([])
  })

  test('opens focused and closing on Escape without the fullscreen layout', async ($, on) => {
    const p: Panes = { open: [], opened: [] }
    panes(on, p)
    await $.command.run(run({ isFullscreen: false, columns: 100 }))
    expect(p.opened[0]?.focus).toBe(true)
    expect(p.opened[0]?.closeOnEscape).toBe(true)
  })
})
