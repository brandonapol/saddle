// The /saddle catchup command (#426): it runs `saddle catchup`, with the
// time as --since, and shows the bullets saddle prints. Run with
// `claude plugin test plugin`.
import { describe, expect, test } from 'claude-code/testing'

import { USAGE, parseSaddle, replyOf } from './commands'

const bullets = ['Since Mon 09:00:', '- 1 task needs you: t4', '- t2 landed', '- 3 agents working'].join('\n')

describe('/saddle catchup', () => {
  test('runs saddle catchup from the last look', () => {
    expect(parseSaddle('catchup', null)).toMatchObject({ argv: ['catchup'] })
  })

  test('passes a time as --since', () => {
    expect(parseSaddle('catchup 2h', null)).toMatchObject({ argv: ['catchup', '--since', '2h'] })
  })

  test('takes at most one time', () => {
    const p = parseSaddle('catchup 2h 3h', null)
    expect('usage' in p && p.usage).toMatch(/^\/saddle catchup takes one optional time/)
  })

  test('shows the catch-up bullets', () => {
    const reply = replyOf(['catchup'], { text: bullets + '\n' })
    expect(reply).toBe(bullets)
    expect(reply).toContain('- 1 task needs you: t4')
  })

  test('the usage names it', () => {
    expect(USAGE).toContain('catchup [since]')
  })
})
