// The instant /saddle commands (#169): `/saddle status`, `land`, `hold
// <task>`, `release <task>`, `restack`, `catchup [since]` and `automerge on|off|status`. Each
// runs one saddle command and shows a short reply, with no Claude turn, even
// while one is in flight. register.tsx registers /saddle and runs what
// parseSaddle returns; nothing here runs saddle.
import type { SaddleSnapshot } from '../types'

/** The hint drawn after /saddle in the typeahead. */
export const ARGUMENT_HINT = 'status | land | hold <task> | release <task> | restack | catchup [since] | automerge on|off|status'

export const USAGE = [
  'Usage: /saddle <command>',
  '  status                   list tasks, claims and the merge train',
  '  land                     run the merge train',
  '  hold <task> [reason]     keep a task from landing or auto-merging',
  '  release <task>           let a held task land or auto-merge again',
  '  restack                  rebuild landed stacks',
  '  catchup [since]          what happened since you last looked, or since 2h / a time',
  '  automerge on|off|status  turn auto-merge of ready PR stacks on or off, or show it',
].join('\n')

/** The most output lines a reply shows before cutting it short. */
export const REPLY_LINES = 30

const SHORT_MS = 30000
/** Landing rebases and runs the repo's check for each queued branch. */
const LAND_MS = 600000
/** Restacking rebases every landed stack. */
const RESTACK_MS = 300000

export type Parsed = { argv: string[]; timeoutMs: number } | { usage: string }

function noArgs(name: string, rest: string[], argv: string[], timeoutMs = SHORT_MS): Parsed {
  return rest.length ? { usage: `/saddle ${name} takes no arguments\n${USAGE}` } : { argv, timeoutMs }
}

// via says where hold and release act, as the needs-you band decides: a
// task in the train's queue holds there, one only in a PR stack holds its
// stack's auto-merge, and anything else goes to the queue, which says why not.
function via(task: string, s: SaddleSnapshot | null): 'queue' | 'automerge' {
  if (s?.queue.some(q => q.task === task)) {
    return 'queue'
  }
  if (s?.stack?.stacks?.some(g => g.nodes.some(n => n.task === task))) {
    return 'automerge'
  }
  return 'queue'
}

/** What `/saddle <args>` runs, or the usage to show instead. */
export function parseSaddle(args: string, s: SaddleSnapshot | null): Parsed {
  const [name = '', ...rest] = args.trim().split(/\s+/).filter(Boolean)
  switch (name) {
    case '':
    case 'help':
      return { usage: USAGE }
    case 'status':
      return noArgs(name, rest, ['status'])
    case 'land':
      return noArgs(name, rest, ['land'], LAND_MS)
    case 'restack':
      return noArgs(name, rest, ['restack'], RESTACK_MS)
    case 'catchup': {
      const [since, ...more] = rest
      if (more.length) {
        return { usage: `/saddle catchup takes one optional time\n${USAGE}` }
      }
      return { argv: since ? ['catchup', '--since', since] : ['catchup'], timeoutMs: SHORT_MS }
    }
    case 'automerge': {
      const [sub = 'status', ...more] = rest
      if (!['on', 'off', 'status'].includes(sub) || more.length) {
        return { usage: `/saddle automerge takes on, off or status\n${USAGE}` }
      }
      return { argv: ['automerge', sub], timeoutMs: SHORT_MS }
    }
    case 'hold': {
      const [task, ...reason] = rest
      if (!task) {
        return { usage: `/saddle hold needs a task\n${USAGE}` }
      }
      if (via(task, s) === 'automerge') {
        return { argv: ['automerge', 'hold', task], timeoutMs: SHORT_MS }
      }
      return { argv: ['queue', 'hold', task, reason.length ? reason.join(' ') : 'held with /saddle hold'], timeoutMs: SHORT_MS }
    }
    case 'release': {
      const [task, ...more] = rest
      if (!task) {
        return { usage: `/saddle release needs a task\n${USAGE}` }
      }
      if (more.length) {
        return { usage: `/saddle release takes one task\n${USAGE}` }
      }
      return { argv: [via(task, s), 'release', task], timeoutMs: SHORT_MS }
    }
    default:
      return { usage: `Unknown /saddle command "${name}"\n${USAGE}` }
  }
}

/** The reply a run shows: its output, cut short, or what failed. */
export function replyOf(argv: string[], ran: { text: string } | { error: string }): string {
  if ('error' in ran) {
    return ran.error
  }
  const text = ran.text.replace(/^\s*\n/, '').trimEnd()
  if (!text.trim()) {
    return `saddle ${argv.join(' ')}: done`
  }
  const lines = text.split('\n')
  if (lines.length <= REPLY_LINES) {
    return text
  }
  const more = lines.length - REPLY_LINES
  return [...lines.slice(0, REPLY_LINES), `… ${more} more line${more === 1 ? '' : 's'} (run saddle ${argv.join(' ')} in a shell)`].join(
    '\n',
  )
}
