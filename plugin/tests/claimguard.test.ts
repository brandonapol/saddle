// The claim guard (#171): which paths a claim covers, which paths a Bash
// line writes, and the guard in a worker session: allowed and held calls
// against claim fixtures, and each way past a hold (claim, retry, Proceed,
// saddle down). Run with `claude plugin test plugin`.
import { describe, expect, mock, test } from 'claude-code/testing'
import type { On, RenderPropsOf } from 'claude-code'
import type { Engine } from 'claude-code/testing'

import { TOOLS, bashTargets, claimed, expandBraces, heldMessage, inWorktree, matchClaim } from '../hooks/claimguard'
import type { SaddleGuard } from '../types'
import { memoryState } from './fixtures'

const ROOT = '/repo/.saddle/worktrees/t9'
const CLAIMS = ['plugin/hooks/claimguard*', 'plugin/tests/claimguard*', 'docs/guard.md', 'internal/**/*_test.go']

describe('matchClaim', () => {
  test('a plain path covers itself and everything under it', () => {
    expect(matchClaim('docs/guard.md', 'docs/guard.md')).toBe(true)
    expect(matchClaim('internal/hook', 'internal/hook/hook.go')).toBe(true)
    expect(matchClaim('internal/hook/', './internal/hook/a/b.go')).toBe(true)
    expect(matchClaim('internal/hook', 'internal/hooks.go')).toBe(false)
  })

  test('* stays in its segment', () => {
    expect(matchClaim('plugin/hooks/claimguard*', 'plugin/hooks/claimguard.tsx')).toBe(true)
    expect(matchClaim('plugin/hooks/claimguard*', 'plugin/hooks/claimguard/x.ts')).toBe(false)
    expect(matchClaim('plugin/hooks/claimguard*', 'plugin/hooks/register.tsx')).toBe(false)
    expect(matchClaim('plugin/*.md', 'plugin/README.md')).toBe(true)
  })

  test('** spans any number of segments, none included', () => {
    expect(matchClaim('plugin/**', 'plugin/a/b/c.ts')).toBe(true)
    expect(matchClaim('internal/**/*_test.go', 'internal/hook_test.go')).toBe(true)
    expect(matchClaim('internal/**/*_test.go', 'internal/a/b/x_test.go')).toBe(true)
    expect(matchClaim('internal/**/*_test.go', 'internal/a/x.go')).toBe(false)
  })

  test('?, classes and braces', () => {
    expect(matchClaim('a/?.go', 'a/x.go')).toBe(true)
    expect(matchClaim('a/?.go', 'a/xy.go')).toBe(false)
    expect(matchClaim('a/[ab].go', 'a/b.go')).toBe(true)
    expect(matchClaim('a/[!ab].go', 'a/b.go')).toBe(false)
    expect(matchClaim('{docs,plugin}/x.md', 'plugin/x.md')).toBe(true)
    expect(matchClaim('{docs,plugin}/x.md', 'internal/x.md')).toBe(false)
    expect(matchClaim('a.go', 'a+go')).toBe(false)
    expect(expandBraces('a/{b,c/{d,e}}.go')).toEqual(['a/b.go', 'a/c/d.go', 'a/c/e.go'])
  })

  test('claimed checks every glob', () => {
    expect(claimed(CLAIMS, 'docs/guard.md')).toBe(true)
    expect(claimed(CLAIMS, 'docs/other.md')).toBe(false)
    expect(claimed([], 'docs/guard.md')).toBe(false)
  })
})

describe('inWorktree', () => {
  test('relative to the worktree, null outside it or in .git', () => {
    expect(inWorktree(ROOT, `${ROOT}/plugin/x.ts`)).toBe('plugin/x.ts')
    expect(inWorktree(ROOT, 'plugin/./a/../x.ts')).toBe('plugin/x.ts')
    expect(inWorktree(ROOT, `${ROOT}/../t8/plugin/x.ts`)).toBeNull()
    expect(inWorktree(ROOT, '/tmp/scratch/x')).toBeNull()
    expect(inWorktree(ROOT, '../x')).toBeNull()
    expect(inWorktree(ROOT, `${ROOT}/.git/config`)).toBeNull()
    expect(inWorktree(ROOT, ROOT)).toBeNull()
  })
})

describe('bashTargets', () => {
  const cases: [string, string[]][] = [
    ['echo hi > docs/a.md', ['docs/a.md']],
    ['echo hi >> docs/a.md 2>&1', ['docs/a.md']],
    ['go test ./... 2> out.log', ['out.log']],
    ['cat x | tee -a one.txt "two words.txt"', ['one.txt', 'two words.txt']],
    ['touch a b && rm -f c', ['a', 'b', 'c']],
    ['cp -r src dst/ ; mv old new', ['dst', 'new']],
    ["sed -i 's/a/b/' f1 f2", ['f1', 'f2']],
    ['sed -i.bak -e s/a/b/ f1', ['f1']],
    ["sed 's/a/b/' f1", []],
    ['go test ./... >/dev/null', []],
    ['echo $x > "$OUT"', []],
    ['rm *.tmp', []],
    ['cd sub && touch a', []],
    ['touch a; cd sub; touch b', ['a']],
    ['git status && ls -la', []],
    ["echo 'unbalanced > x", []],
  ]
  for (const [line, want] of cases) {
    test(line, () => {
      expect(bashTargets(line).map(p => p.replace(/\/$/, ''))).toEqual(want)
    })
  }
})

describe('heldMessage', () => {
  test('names the path, the claims and every way out', () => {
    const m = heldMessage('t9', ['docs/other.md'], ['docs/guard.md'])
    expect(m).toContain('docs/other.md is outside t9')
    expect(m).toContain('(docs/guard.md)')
    expect(m).toContain('claim ["docs/other.md"]')
    expect(m).toContain('retry the same call unchanged')
    expect(m).toContain('Proceed')
  })
})

type World = {
  /** The task's claims as saddle reports them; null makes status fail. */
  claims: string[] | null
  /** Calls that reached the tool. */
  ran: string[]
  /** saddle commands run. */
  calls: string[]
  prompts: string[]
}

// world stands a worker session up beneath the plugin: its env, saddle's
// status, the tools themselves and $.state.
function world(on: On, w: World, env: Record<string, string> = { SADDLE_TASK: 't9' }) {
  const state = memoryState(on)
  mock.env(on, env)
  on('session.cwd', () => ({ value: ROOT }))
  on('process.run', ($, e) => {
    w.calls.push(e.argv.join(' '))
    if (w.claims === null) {
      return { value: { exitCode: 1, stdout: '', stderr: 'no saddle here', isStdoutTruncated: false, isStderrTruncated: false } }
    }
    const tasks = [
      { id: 't8', title: 'other', status: 'running', claims: ['docs/**'], worktree: '/repo/.saddle/worktrees/t8' },
      { id: 't9', title: 'guarded', status: 'running', claims: w.claims, worktree: ROOT },
    ]
    return {
      value: {
        exitCode: 0,
        stdout: JSON.stringify({ integration: 'saddle/integration', tasks }),
        stderr: '',
        isStdoutTruncated: false,
        isStderrTruncated: false,
      },
    }
  })
  on('prompt.submit', ($, e) => {
    w.prompts.push(e.text)
    return { text: e.text }
  })
  on('tool.call', { tool: TOOLS }, ($, e) => {
    w.ran.push(`${e.tool} ${e.tool === 'Bash' ? e.command : e.tool === 'NotebookEdit' ? e.notebook_path : e.file_path}`)
    return { result: 'ok' as never }
  })
  return { pending: () => (state.get('saddle/guard')?.value as SaddleGuard | undefined)?.pending }
}

const fresh = (claims: string[] | null = CLAIMS): World => ({ claims, ran: [], calls: [], prompts: [] })
const on_ = { options: { claimGuard: true } }

const edit = ($: Engine, path: string) =>
  $.tool.call({ tool: 'Edit', file_path: path.startsWith('/') ? path : `${ROOT}/${path}`, old_string: 'a', new_string: 'b' })
const write = ($: Engine, path: string) => $.tool.call({ tool: 'Write', file_path: `${ROOT}/${path}`, content: 'x' })

describe('the guard', () => {
  test('lets calls on claimed paths through', on_, async ($, on) => {
    const w = fresh()
    world(on, w)
    expect((await edit($, 'plugin/hooks/claimguard.tsx')).deny).toBeUndefined()
    expect((await write($, 'docs/guard.md')).deny).toBeUndefined()
    expect((await $.tool.call({ tool: 'NotebookEdit', notebook_path: `${ROOT}/internal/x/y_test.go`, new_source: '' })).deny).toBeUndefined()
    expect((await $.tool.call({ tool: 'Bash', command: 'echo hi > docs/guard.md' })).deny).toBeUndefined()
    expect(w.ran).toHaveLength(4)
    // One status read serves every call its claims cover.
    expect(w.calls).toEqual(['saddle status --json'])
  })

  test('lets paths outside the worktree through: saddle’s own hook rules on those', on_, async ($, on) => {
    const w = fresh()
    world(on, w)
    expect((await edit($, '/tmp/scratch/notes.md')).deny).toBeUndefined()
    expect((await edit($, '/repo/.saddle/worktrees/t8/docs/x.md')).deny).toBeUndefined()
    expect((await $.tool.call({ tool: 'Bash', command: 'go test ./... && git status' })).deny).toBeUndefined()
    expect(w.ran).toHaveLength(3)
  })

  for (const [name, call] of [
    ['Edit', ($: Engine) => edit($, 'plugin/hooks/register.tsx')],
    ['Write', ($: Engine) => write($, 'plugin/hooks/register.tsx')],
    ['NotebookEdit', ($: Engine) => $.tool.call({ tool: 'NotebookEdit', notebook_path: `${ROOT}/plugin/hooks/register.tsx`, new_source: '' })],
    ['Bash', ($: Engine) => $.tool.call({ tool: 'Bash', command: "sed -i 's/a/b/' plugin/hooks/register.tsx" })],
  ] as const) {
    test(`holds ${name} on an unclaimed path and asks for a claim`, on_, async ($, on) => {
      const w = fresh()
      const s = world(on, w)
      const r = await call($)
      expect(r.deny).toContain('plugin/hooks/register.tsx is outside t9')
      expect(r.deny).toContain('claim ["plugin/hooks/register.tsx"]')
      expect(w.ran).toEqual([])
      expect(s.pending()).toEqual([{ path: 'plugin/hooks/register.tsx', tool: name }])
    })
  }

  test('reads the claims again before holding, so a fresh claim lets the call through', on_, async ($, on) => {
    const w = fresh()
    world(on, w)
    expect((await edit($, 'docs/guard.md')).deny).toBeUndefined()
    w.claims = [...CLAIMS, 'plugin/hooks/register.tsx']
    expect((await edit($, 'plugin/hooks/register.tsx')).deny).toBeUndefined()
    expect(w.calls).toHaveLength(2)
    expect(w.ran).toHaveLength(2)
  })

  test('escape hatch: a retried call on a held path goes through', on_, async ($, on) => {
    const w = fresh()
    world(on, w)
    expect((await edit($, 'README.md')).deny).toBeDefined()
    expect((await edit($, 'README.md')).deny).toBeUndefined()
    expect(w.ran).toEqual([`Edit ${ROOT}/README.md`])
  })

  test('escape hatch: fails open when saddle cannot say what the task claimed', on_, async ($, on) => {
    const w = fresh(null)
    world(on, w)
    expect((await edit($, 'README.md')).deny).toBeUndefined()
    expect(w.ran).toHaveLength(1)
  })

  test('is off unless the claimGuard option is on', async ($, on) => {
    const w = fresh()
    world(on, w)
    expect((await edit($, 'README.md')).deny).toBeUndefined()
    expect(w.calls).toEqual([])
  })

  test('leaves the orchestrator’s session alone', on_, async ($, on) => {
    const w = fresh()
    world(on, w, {})
    expect((await edit($, 'README.md')).deny).toBeUndefined()
    expect(w.calls).toEqual([])
  })
})

function props(): RenderPropsOf['AbovePrompt'] {
  return { hasSurvey: false, isWorking: false, maxRows: 20, bodyColumns: 100, scroll: { offset: 0, bodyRows: 20 }, view: {} }
}

describe('the band', () => {
  for (const surface of ['terminal', 'desktop'] as const) {
    test(`shows a held call, and Proceed tells the worker it may go ahead (${surface})`, on_, async ($, on) => {
      const w = fresh()
      world(on, w)
      on('ui.render', { component: 'AbovePrompt' }, ($, e) => $.ui.resolve(e).Box({ key: 'engine-band' }))
      await edit($, 'README.md')
      const ui = await $.ui.mount({ plugin: 'saddle', surface, component: 'AbovePrompt', props: props() })
      expect((await ui.find({ key: 'held-text-README.md' }))?.text).toBe('⏸ claim guard held Edit on README.md (unclaimed)')
      await ui.press({ key: 'proceed-README.md' })
      await ui.redraw()
      expect(w.prompts).toHaveLength(1)
      expect(w.prompts[0]).toContain('let README.md through the claim guard')
      expect(await ui.find({ key: 'claimguard' })).toBeUndefined()
      expect((await edit($, 'README.md')).deny).toBeUndefined()
      await ui.unmount()
    })

    test(`Dismiss hides the hold without a prompt (${surface})`, on_, async ($, on) => {
      const w = fresh()
      world(on, w)
      on('ui.render', { component: 'AbovePrompt' }, ($, e) => $.ui.resolve(e).Box({ key: 'engine-band' }))
      await edit($, 'README.md')
      const ui = await $.ui.mount({ plugin: 'saddle', surface, component: 'AbovePrompt', props: props() })
      await ui.press({ key: 'dismiss-README.md' })
      await ui.redraw()
      expect(w.prompts).toEqual([])
      expect(await ui.find({ key: 'claimguard' })).toBeUndefined()
      await ui.unmount()
    })
  }

  test('draws nothing while nothing is held', on_, async ($, on) => {
    world(on, fresh())
    on('ui.render', { component: 'AbovePrompt' }, ($, e) => $.ui.resolve(e).Box({ key: 'engine-band' }))
    const ui = await $.ui.mount({ plugin: 'saddle', surface: 'terminal', component: 'AbovePrompt', props: props() })
    expect(await ui.find({ key: 'claimguard' })).toBeUndefined()
    expect(await ui.find({ key: 'engine-band' })).toBeDefined()
    await ui.unmount()
  })
})
