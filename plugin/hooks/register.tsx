// The saddle mod's hooks module (#165). It keeps the shared snapshot fresh,
// draws it in the saddle pane (#167) and draws the needs-you band above the
// prompt (#168); the instant /saddle commands (#169) read it too. A timer runs
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
import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { SaddlePaneTab, SaddleSnapshot } from '../types'
import { ARGUMENT_HINT, parseSaddle, replyOf } from './commands'
import { ACTION_LABELS, actionArgv, askPrompt, needLine, needsYou } from './needsyou'
import type { Need, NeedAction } from './needsyou'
import { PINNED_ROWS, TABS, headerOf, scrolled, tabLines } from './pane'
import type { Tone } from './pane'
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

// The pane's view: which tab, and the first body row shown.
const paneRef = { plugin: 'saddle', key: 'pane' } as const
const pane = atom(paneRef, { tab: 'agents', offset: 0 })

// The needs-you band's view: the screen Peek fetched, shown under the items.
const bandRef = { plugin: 'saddle', key: 'band' } as const
const band = atom(bandRef, { peek: null })

/** The pane's id: `$.ui.open`, its `ui.render` requestId and `ui.scroll`. */
const PANE = 'saddle'
const COMMAND = 'saddle-pane'
/** The instant commands: `/saddle status`, `/saddle land` and the rest. */
const SADDLE = 'saddle'
/** Rows a wheel tick moves the body, as the /diff pane moves its hunks. */
const WHEEL_ROWS = 3

function toneProps(tone: Tone | undefined) {
  switch (tone) {
    case 'dim':
      return { dimColor: true }
    case 'warn':
      return { color: 'warning' }
    case 'bad':
      return { color: 'error' }
    case 'good':
      return { color: 'success' }
    case 'head':
      return { bold: true }
    default:
      return {}
  }
}

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

// press runs a band button: Peek fetches the task's screen into the band
// (or closes it when it already shows), Ask Claude hands the item to the
// orchestrator as a prompt, and the rest run their saddle command and toast
// its first line. The next refresh redraws the band with the outcome.
async function press($: EngineInterface, cwd: string, n: Need, action: NeedAction) {
  if (action === 'ask') {
    $.ui.toast(`Asked Claude about ${n.task}`)
    await $.prompt.submit({ text: askPrompt(n) })
    return
  }
  const argv = actionArgv(n, action)
  if (!argv) {
    return
  }
  if (action === 'peek' && (await read($, band)).peek?.task === n.task) {
    await update($, band, () => ({ peek: null }))
    return
  }
  const r = await saddle($, cwd, argv, action === 'land' ? 600000 : 10000)
  if (action === 'peek') {
    const screen = 'text' in r ? r.text.replace(/\s+$/, '') : r.error
    await update($, band, () => ({ peek: { task: n.task, screen } }))
    return
  }
  $.ui.toast('text' in r ? (r.text.trim().split('\n')[0] ?? `saddle ${argv[0]}: done`) : r.error)
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
    await $.command.register({ name: COMMAND, description: "Show or hide saddle's agents, claims, merge train and stacks" })
    await $.command.register({
      name: SADDLE,
      description: 'Run a saddle command now, without a Claude turn: status, land, hold, release, restack, automerge',
      argumentHint: ARGUMENT_HINT,
      immediate: true,
    })
    // A refresh that throws is retried by the next tick; the engine logs it.
    const tick = () => void refresh($, e.cwd).catch(() => undefined)
    $.clock.every(POLL_MS, tick)
    tick()

    return started
  })

  // /saddle-pane toggles the pane. Without the fullscreen layout it opens
  // inline as a dialog, focused and closing on Escape, as /diff's does.
  on('command.run', { command: COMMAND }, async ($, e) => {
    if ((await $.ui.panes()).some(p => p.id === PANE)) {
      await $.ui.close({ id: PANE })
      return { text: 'Saddle pane hidden' }
    }
    const opened = e.presentation.isFullscreen
      ? await $.ui.open({ id: PANE, title: 'saddle' })
      : await $.ui.open({ id: PANE, title: 'saddle', focus: true, closeOnEscape: true })
    return { text: opened.isPlaced ? 'Saddle pane shown' : `Saddle pane waits: ${opened.reason}` }
  })

  // /saddle <command> runs at once, even mid-turn: one saddle command, its
  // short reply printed as the command's output. It reads the snapshot only
  // to tell a queued task's hold from a PR stack's.
  on('command.run', { command: SADDLE }, async ($, e) => {
    const p = parseSaddle(e.args, await read($, snapshot))
    if ('usage' in p) {
      return { text: p.usage }
    }
    return { text: replyOf(p.argv, await saddle($, await $.session.cwd(), p.argv, p.timeoutMs)) }
  })

  // The header and tabs stay put; the body is the plugin's own window over
  // the tab's rows, moved here rather than by the engine.
  on('ui.scroll', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const snap = await read($, snapshot)
    const by = e.pointer && Math.abs(e.by) === 1 ? e.by * WHEEL_ROWS : e.by
    const room = Math.max(1, e.bodyRows - PINNED_ROWS)
    await update($, pane, v => ({ ...v, offset: scrolled(v.offset, by, tabLines(snap, v.tab).length, room) }))
    return {}
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Text, Button } = $.ui.resolve(e)
    const snap = await read($, snapshot)
    const view = await read($, pane)
    const lines = tabLines(snap, view.tab)
    const room = Math.max(1, e.props.scroll.bodyRows - PINNED_ROWS)
    const offset = scrolled(view.offset, 0, lines.length, room)
    const show = (tab: SaddlePaneTab) => () => void update($, pane, () => ({ tab, offset: 0 }))

    return (
      <Box flexDirection="column">
        <Box key="header">
          <Text bold wrap="truncate-end">
            {headerOf(snap)}
          </Text>
        </Box>
        <Box flexDirection="row" gap={1}>
          {TABS.map(t => (
            <Button
              key={`tab-${t.tab}`}
              hotkey={t.hotkey}
              label={t.label}
              {...(t.tab === view.tab ? { variant: 'primary' as const } : {})}
              onPress={show(t.tab)}
            />
          ))}
        </Box>
        {lines.slice(offset, offset + room).map(l => (
          <Text wrap="truncate-end" {...toneProps(l.tone)}>
            {l.text}
          </Text>
        ))}
      </Box>
    )
  })

  // The needs-you band: one line per task waiting on someone, with its
  // buttons, and the screen Peek fetched under them. It passes, drawing
  // nothing, while nothing is pending or a survey holds the band.
  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const snap = await read($, snapshot)
    const needs = needsYou(snap)
    if (e.props.hasSurvey || needs.length === 0) {
      return next(e)
    }
    const { Box, Text, Button } = $.ui.resolve(e)
    const { peek } = await read($, band)
    const shown = peek && needs.some(n => n.task === peek.task) ? peek : null
    const cwd = await $.session.cwd()

    return (
      <Box key="needsyou" flexDirection="column">
        {needs.map(n => (
          <Box key={`row-${n.task}`} flexDirection="row" gap={1}>
            <Box key={`need-${n.task}`}>
              <Text color="warning" wrap="truncate-end">
                {needLine(n)}
              </Text>
            </Box>
            {n.actions.map(a => (
              <Button
                key={`${n.task}-${a}`}
                label={ACTION_LABELS[a]}
                dimColor={a !== 'ask'}
                onPress={() => press($, cwd, n, a)}
              />
            ))}
          </Box>
        ))}
        {shown ? (
          <Box key="peek" flexDirection="column">
            <Box flexDirection="row" gap={1}>
              <Box key="peek-title">
                <Text bold>{`${shown.task}'s screen`}</Text>
              </Box>
              <Button key="peek-close" label="Close" role="dismiss" onPress={() => update($, band, () => ({ peek: null }))} />
            </Box>
            {shown.screen.split('\n').map(l => (
              <Text dimColor wrap="truncate-end">
                {l}
              </Text>
            ))}
          </Box>
        ) : null}
      </Box>
    )
  })
}
