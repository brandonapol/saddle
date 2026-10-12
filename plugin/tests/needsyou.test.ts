// The needs-you band (#168): which tasks it lists for each event kind, the
// buttons each gets and the saddle command each button runs, and the band
// drawn above the prompt on the terminal and the desktop. Run with `claude
// plugin test plugin`.
import { describe, expect, test } from 'claude-code/testing'
import type { On, RenderPropsOf } from 'claude-code'
import type { Engine } from 'claude-code/testing'

import type { SaddleSnapshot } from '../types'
import { actionArgv, askPrompt, needLine, needsYou } from '../hooks/needsyou'
import { idle } from '../hooks/snapshot'
import { fleet, loaded, memoryState } from './fixtures'

// crowd is a fleet with one task for each thing that needs a person, and a
// few that don't.
const crowd: SaddleSnapshot = {
  engine: 'engine',
  isLive: true,
  status: {
    integration: 'saddle/integration',
    tasks: [
      { id: 't0', title: 'orchestrator', status: 'idle' },
      { id: 't10', title: 'Answer me', status: 'needs_you', branch: 'saddle/t10', window: '@10' },
      { id: 't11', title: 'Went quiet', status: 'idle', branch: 'saddle/t11', window: '@11' },
      {
        id: 't12',
        title: 'Rebase trouble',
        status: 'conflict',
        branch: 'saddle/t12',
        window: '@12',
        train: 'conflict: rebase failed',
      },
      {
        id: 't13',
        title: 'Gate failed',
        status: 'conflict',
        branch: 'saddle/t13',
        window: '@13',
        train: 'test_failed: tests failed',
      },
      { id: 't14', title: 'Red on main', status: 'landed', branch: 'saddle/t14' },
      { id: 't16', title: 'Risky stack', status: 'landed', branch: 'saddle/t16' },
      { id: 't17', title: 'Parked', status: 'done', branch: 'saddle/t17', train: 'on_hold: waiting on review' },
      { id: 't18', title: 'Busy', status: 'running', branch: 'saddle/t18', window: '@18' },
      { id: 't19', title: 'Gone', status: 'killed', branch: 'saddle/t19' },
      { id: 't20', title: 'Held stack', status: 'landed', branch: 'saddle/t20' },
      {
        id: 't21',
        title: 'A title long enough that the band has to cut it short',
        status: 'needs_you',
        branch: 'saddle/t21',
      },
    ],
    ci_red: [{ task: 't14', pr: 'https://github.com/o/r/pull/66', checks: ['ci / test'], held: ['t15'], repair: 't22', attempts: 2 }],
  },
  queue: [
    { position: 1, task: 't12', state: 'conflict', note: 'rebase failed', seq: 1 },
    { position: 2, task: 't13', state: 'test_failed', note: 'tests failed', seq: 2 },
    { position: 3, task: 't17', state: 'on_hold', note: 'waiting on review', seq: 3 },
  ],
  stack: {
    enabled: true,
    source: 'runtime',
    stacks: [
      {
        id: 't16',
        nodes: [{ task: 't16', pr: '#70', base: 'main', checks: 'ChecksPass', at_risk: 'red CI below' }],
        next: '#70',
        ready: false,
        why: 'at risk',
      },
      {
        id: 't20',
        nodes: [{ task: 't20', pr: '#71', base: 'main', checks: 'ChecksFail' }],
        held: true,
        next: '#71',
        ready: false,
        why: 'held',
      },
    ],
  },
  errors: [],
  changedAt: 1,
}

// calm is a live fleet where nothing waits on anyone.
const calm: SaddleSnapshot = {
  ...fleet,
  status: { integration: 'x', tasks: [{ id: 't2', title: 'x', status: 'running', branch: 'b' }] },
  queue: [{ position: 1, task: 't2', state: 'queued', seq: 1 }],
  stack: { enabled: true, source: 'runtime' },
}

const byTask = (task: string) => needsYou(crowd).find(n => n.task === task)

describe('needsYou', () => {
  test('lists one line per task that needs a person, in fleet order', () => {
    expect(needsYou(crowd).map(needLine)).toEqual([
      '‼ t10 (Answer me): waiting on a prompt',
      '‼ t11 (Went quiet): stopped without calling done',
      '‼ t12 (Rebase trouble): conflicted: rebase failed',
      '‼ t13 (Gate failed): tests failed in the train',
      '‼ t14 (Red on main): CI red on pull/66: ci / test (t22 repairing, attempt 2; holds t15)',
      '‼ t16 (Risky stack): at risk: red CI below (#70)',
      '‼ t17 (Parked): held in the train: waiting on review',
      '‼ t20 (Held stack): CI failing on #71',
      '‼ t21 (A title long enough that the band has t…): waiting on a prompt',
    ])
  })

  test('names the kind of each', () => {
    expect(needsYou(crowd).map(n => [n.task, n.kind])).toEqual([
      ['t10', 'prompt'],
      ['t11', 'stalled'],
      ['t12', 'conflict'],
      ['t13', 'ci'],
      ['t14', 'ci'],
      ['t16', 'at_risk'],
      ['t17', 'held'],
      ['t20', 'ci'],
      ['t21', 'prompt'],
    ])
  })

  test('offers peek where there is a screen, and land, hold or release where they apply', () => {
    expect(byTask('t10')?.actions).toEqual(['peek', 'ask'])
    expect(byTask('t11')?.actions).toEqual(['peek', 'ask'])
    expect(byTask('t12')?.actions).toEqual(['peek', 'ask', 'land', 'hold'])
    expect(byTask('t13')?.actions).toEqual(['peek', 'ask', 'land', 'hold'])
    expect(byTask('t14')?.actions).toEqual(['ask'])
    expect(byTask('t16')?.actions).toEqual(['ask', 'hold'])
    expect(byTask('t17')?.actions).toEqual(['ask', 'release'])
    expect(byTask('t20')?.actions).toEqual(['ask', 'release'])
    expect(byTask('t21')?.actions).toEqual(['ask'])
  })

  test('is empty when nothing is pending or saddle is not running', () => {
    expect(needsYou(null)).toEqual([])
    expect(needsYou(idle())).toEqual([])
    expect(needsYou({ ...crowd, isLive: false })).toEqual([])
    expect(needsYou(calm)).toEqual([])
  })
})

describe('actionArgv', () => {
  test('runs the train, queue and auto-merge commands that fit', () => {
    const argv = (task: string, action: Parameters<typeof actionArgv>[1]) => {
      const n = byTask(task)
      return n ? actionArgv(n, action) : undefined
    }
    expect(argv('t10', 'peek')).toEqual(['peek', 't10', '-n', '15'])
    expect(argv('t12', 'land')).toEqual(['land'])
    expect(argv('t12', 'hold')).toEqual(['queue', 'hold', 't12', 'held from the needs-you band'])
    expect(argv('t17', 'release')).toEqual(['queue', 'release', 't17'])
    expect(argv('t16', 'hold')).toEqual(['automerge', 'hold', 't16'])
    expect(argv('t20', 'release')).toEqual(['automerge', 'release', 't20'])
    expect(argv('t10', 'ask')).toBeUndefined()
  })
})

describe('askPrompt', () => {
  test('hands the item to the orchestrator as a [saddle] prompt', () => {
    const n = byTask('t10')
    expect(n && askPrompt(n)).toBe(
      '[saddle] t10 (Answer me) needs you: waiting on a prompt. The person pressed Ask Claude in the needs-you band: ' +
        'look into it (peek it, read its notices) and handle it, or tell them what you need from them.',
    )
  })
})

function props(): RenderPropsOf['AbovePrompt'] {
  return { hasSurvey: false, isWorking: false, maxRows: 20, bodyColumns: 100, scroll: { offset: 0, bodyRows: 20 }, view: {} }
}

type Ran = { argv: string[]; cwd?: string }

// beneath answers for the engine under the plugin: its own (empty) band,
// drawn when the plugin passes, and the session's directory.
function beneath(on: On) {
  on('ui.render', { component: 'AbovePrompt' }, ($, e) => $.ui.resolve(e).Box({ key: 'engine-band' }))
  on('session.cwd', () => ({ value: '/repo' }))
}

// mountBand draws the band over `snap`, held in $.state, with saddle's
// commands, the prompt and toasts stood up beneath the plugin. As in the
// pane's tests, a write doesn't redraw by itself here: press redraws after.
async function mountBand($: Engine, on: On, surface: 'terminal' | 'desktop', snap: SaddleSnapshot | null, extra: Partial<RenderPropsOf['AbovePrompt']> = {}) {
  memoryState(on, { 'saddle/snapshot': snap })
  beneath(on)
  const ran: Ran[] = []
  const prompts: string[] = []
  const toasts: string[] = []
  on('process.run', ($, e) => {
    ran.push({ argv: [...e.argv] })
    const stdout = e.argv[1] === 'peek' ? 'Do you want to proceed?\n❯ 1. Yes\n  2. No\n' : `${e.argv.slice(1).join(' ')}: ok\n`
    return { value: { exitCode: 0, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  on('prompt.submit', ($, e) => {
    prompts.push(e.text)
    return { text: e.text }
  })
  on('ui.toast', ($, e) => {
    toasts.push(e.text)
    return { value: undefined }
  })
  const ui = await $.ui.mount({ plugin: 'saddle', surface, component: 'AbovePrompt', props: { ...props(), ...extra } })
  return {
    ui,
    ran,
    prompts,
    toasts,
    press: async (key: string) => {
      await ui.press({ key })
      await ui.redraw()
    },
  }
}

describe('band', () => {
  for (const surface of ['terminal', 'desktop'] as const) {
    test(`lists what needs you, with its buttons (${surface})`, loaded, async ($, on) => {
      const { ui } = await mountBand($, on, surface, crowd)
      expect((await ui.find({ key: 'need-t10' }))?.text).toBe('‼ t10 (Answer me): waiting on a prompt')
      expect(await ui.find({ key: 't10-peek' })).toBeDefined()
      expect(await ui.find({ key: 't10-ask' })).toBeDefined()
      expect(await ui.find({ key: 't10-land' })).toBeUndefined()
      expect(await ui.find({ key: 't12-land' })).toBeDefined()
      expect(await ui.find({ key: 't17-release' })).toBeDefined()
      expect(await ui.find({ key: 'need-t18' })).toBeUndefined()
      await ui.unmount()
    })

    test(`hides when nothing is pending (${surface})`, loaded, async ($, on) => {
      const { ui } = await mountBand($, on, surface, calm)
      expect(await ui.find({ key: 'needsyou' })).toBeUndefined()
      await ui.unmount()
    })
  }

  const quiet: [string, SaddleSnapshot | null, boolean][] = [
    ['before saddle is read', null, false],
    ['while saddle is stopped', idle(), false],
    ['while a survey holds the band', crowd, true],
  ]
  for (const [when, snap, hasSurvey] of quiet) {
    test(`hides ${when}`, loaded, async ($, on) => {
      const { ui } = await mountBand($, on, 'terminal', snap, { hasSurvey })
      expect(await ui.find({ key: 'needsyou' })).toBeUndefined()
      await ui.unmount()
    })
  }

  test('Peek shows the screen under the band, and closes it', loaded, async ($, on) => {
    const band = await mountBand($, on, 'terminal', crowd)
    await band.press('t10-peek')
    expect(band.ran.map(r => r.argv)).toEqual([['saddle', 'peek', 't10', '-n', '15']])
    expect((await band.ui.find({ key: 'peek-title' }))?.text).toBe("t10's screen")
    expect(await band.ui.find({ type: 'Text', text: '❯ 1. Yes' })).toBeDefined()
    await band.press('peek-close')
    expect(await band.ui.find({ key: 'peek-title' })).toBeUndefined()
    await band.ui.unmount()
  })

  test('Ask Claude submits a [saddle] prompt and runs nothing', loaded, async ($, on) => {
    const band = await mountBand($, on, 'terminal', crowd)
    await band.press('t11-ask')
    expect(band.prompts).toHaveLength(1)
    expect(band.prompts[0]).toMatch(/^\[saddle\] t11 \(Went quiet\) needs you: stopped without calling done\./)
    expect(band.ran).toEqual([])
    expect(band.toasts).toEqual(['Asked Claude about t11'])
    await band.ui.unmount()
  })

  test('Land, Hold and Release run their saddle commands and say how it went', loaded, async ($, on) => {
    const band = await mountBand($, on, 'terminal', crowd)
    await band.press('t12-land')
    await band.press('t12-hold')
    await band.press('t17-release')
    await band.press('t16-hold')
    await band.press('t20-release')
    expect(band.ran.map(r => r.argv)).toEqual([
      ['saddle', 'land'],
      ['saddle', 'queue', 'hold', 't12', 'held from the needs-you band'],
      ['saddle', 'queue', 'release', 't17'],
      ['saddle', 'automerge', 'hold', 't16'],
      ['saddle', 'automerge', 'release', 't20'],
    ])
    expect(band.toasts[0]).toBe('land: ok')
    expect(band.toasts[2]).toBe('queue release t17: ok')
    await band.ui.unmount()
  })

  test('says what failed when a command does', loaded, async ($, on) => {
    memoryState(on, { 'saddle/snapshot': crowd })
    beneath(on)
    const toasts: string[] = []
    on('process.run', () => ({
      value: { exitCode: 1, stdout: '', stderr: 'error: t12 is not queued\n', isStdoutTruncated: false, isStderrTruncated: false },
    }))
    on('ui.toast', ($, e) => {
      toasts.push(e.text)
      return { value: undefined }
    })
    const ui = await $.ui.mount({ plugin: 'saddle', surface: 'terminal', component: 'AbovePrompt', props: props() })
    await ui.press({ key: 't12-hold' })
    expect(toasts).toEqual(['saddle queue: exit 1: error: t12 is not queued'])
    await ui.unmount()
  })
})
