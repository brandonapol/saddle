// Fixture snapshots for the pane's tests (#167): a fleet as `saddle status
// --json`, `queue --json` and `stack --json` would report it.
import type { On } from 'claude-code'

import type { SaddleSnapshot } from '../types'

export const fleet: SaddleSnapshot = {
  engine: 'engine',
  isLive: true,
  status: {
    integration: 'saddle/integration',
    tasks: [
      { id: 't1', title: 'Mod scaffold', status: 'landed', model: 'opus' },
      {
        id: 't2',
        title: 'Saddle pane',
        status: 'running',
        model: 'opus',
        claims: ['plugin/**'],
        activity: 'Edit plugin/hooks/pane.ts',
      },
      {
        id: 't3',
        title: 'Needs-you band',
        status: 'needs_you',
        model: 'sonnet',
        claims: ['plugin/hooks/band.ts', 'docs/band.md'],
        pending_notices: 2,
      },
      { id: 't4', title: 'Claim guard', status: 'done', model: 'opus', train: 'queued' },
      { id: 't5', title: 'Broken spawn', status: 'failed', reason: 'worktree exists' },
    ],
  },
  queue: [
    { position: 1, task: 't4', state: 'queued', seq: 7 },
    { position: 2, task: 't6', state: 'on_hold', note: 'waiting on CI', seq: 8, attempts: 2 },
  ],
  stack: {
    enabled: true,
    source: 'runtime',
    holds: ['t9'],
    stacks: [
      {
        id: 't7',
        nodes: [
          { task: 't7', pr: '#41', base: 'main', checks: 'ChecksPass', mergeable: 'MERGEABLE', merge_state: 'CLEAN' },
          {
            task: 't8',
            pr: '#42',
            base: 'saddle/t7',
            checks: 'ChecksFail',
            mergeable: 'CONFLICTING',
            merge_state: 'DIRTY',
            draft: true,
            at_risk: 'red CI',
          },
        ],
        behind: 3,
        next: '#41',
        ready: true,
      },
      {
        id: 't9',
        nodes: [{ task: 't9', pr: '#50', base: 'main', error: 'gh: timeout' }],
        held: true,
        next: '#50',
        ready: false,
        why: 'held',
      },
    ],
  },
  errors: [],
  changedAt: 1000,
}

// memoryState stands the host's $.state up beneath the plugin, in memory,
// honoring ifVersion as the host does, so `update` and redraws work.
export function memoryState(on: On, initial: Record<string, unknown> = {}) {
  const values = new Map<string, { value: unknown; version: number }>()
  for (const [k, value] of Object.entries(initial)) {
    values.set(k, { value, version: 1 })
  }
  on('state.get', ($, e) => {
    const got = values.get(`${e.plugin}/${e.key}`)
    return { value: got ? { value: got.value, version: got.version } : { value: undefined, version: 0 } }
  })
  on('state.set', ($, e) => {
    const k = `${e.plugin}/${e.key}`
    const version = values.get(k)?.version ?? 0
    if (e.ifVersion !== undefined && e.ifVersion !== version) {
      return { value: { isSet: false, version } }
    }
    values.set(k, { value: e.value, version: version + 1 })
    return { value: { isSet: true, version: version + 1 } }
  })
  return values
}
