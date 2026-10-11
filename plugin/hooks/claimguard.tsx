// The claim guard (#171): an opt-in hook for saddle's worker sessions. It
// holds an Edit, Write or NotebookEdit call (and, best effort, an obvious
// Bash write) on a path inside the worktree that the task has not claimed,
// and tells the worker to claim it first. saddle's own PreToolUse hook
// already denies writes into another task's claims; this one catches the
// unclaimed paths that hook lets through, so two workers don't both wander
// into the same file.
//
// It is never the only way out. Claiming the path is the first; a call the
// guard already held once passes when the worker retries it; the band above
// the prompt offers Proceed to the person; and anything the guard cannot
// read (saddle down, the task unknown) lets the call through.
import { atom, read, update } from 'claude-code'
import type { EngineInterface, On, PluginOptions } from 'claude-code'

import type { SaddleGuard } from '../types'

/** The `userConfig` field that turns the guard on. */
export const OPTION = 'claimGuard'

const guardRef = { plugin: 'saddle', key: 'guard' } as const
const guard = atom(guardRef, { pending: [], held: [] })

const hasMeta = (g: string) => /[*?[{]/.test(g)

/** clean normalizes a glob or path to slash form without ./ or a trailing /. */
export function clean(g: string): string {
  g = g.trim().replace(/\\/g, '/').replace(/\/+$/, '')
  if (!hasMeta(g)) {
    g = normalize(g)
  }
  return g.replace(/^\.\//, '')
}

/** normalize resolves . and .. segments in a slash path. */
function normalize(p: string): string {
  const isAbs = p.startsWith('/')
  const out: string[] = []
  for (const s of p.split('/')) {
    if (s === '' || s === '.') {
      continue
    }
    if (s === '..') {
      if (out.length > 0 && out[out.length - 1] !== '..') {
        out.pop()
      } else if (!isAbs) {
        out.push('..')
      }
      continue
    }
    out.push(s)
  }
  return (isAbs ? '/' : '') + out.join('/')
}

/** expandBraces turns a {a,b} glob into one glob per alternative. */
export function expandBraces(g: string): string[] {
  const open = g.indexOf('{')
  if (open < 0) {
    return [g]
  }
  let depth = 0
  let start = open + 1
  const alts: string[] = []
  for (let i = open; i < g.length; i++) {
    const c = g[i]
    if (c === '{') {
      depth++
    } else if (c === '}' && --depth === 0) {
      alts.push(g.slice(start, i))
      const head = g.slice(0, open)
      const tail = g.slice(i + 1)
      return alts.flatMap(a => expandBraces(head + a + tail))
    } else if (c === ',' && depth === 1) {
      alts.push(g.slice(start, i))
      start = i + 1
    }
  }
  return [g] // unbalanced: match it literally
}

/** segRegExp compiles one path segment of a glob: *, ? and [...] stay in it. */
function segRegExp(seg: string): RegExp {
  let re = ''
  for (let i = 0; i < seg.length; i++) {
    const c = seg[i] ?? ''
    if (c === '*') {
      re += '[^/]*'
    } else if (c === '?') {
      re += '[^/]'
    } else if (c === '[') {
      const end = seg.indexOf(']', i + 2)
      if (end < 0) {
        re += '\\['
        continue
      }
      let body = seg.slice(i + 1, end)
      if (body.startsWith('!')) {
        body = `^${body.slice(1)}`
      }
      re += `[${body.replace(/\\/g, '\\\\')}]`
      i = end
    } else {
      re += c.replace(/[.+^${}()|\\\]]/g, '\\$&')
    }
  }
  return new RegExp(`^${re}$`)
}

function matchSegs(g: string[], p: string[]): boolean {
  if (g.length === 0) {
    return p.length === 0
  }
  const [head, ...rest] = g
  if (head === '**') {
    for (let i = 0; i <= p.length; i++) {
      if (matchSegs(rest, p.slice(i))) {
        return true
      }
    }
    return false
  }
  return p.length > 0 && segRegExp(head ?? '').test(p[0] ?? '') && matchSegs(rest, p.slice(1))
}

/**
 * matchClaim reports whether a repo-relative path falls under a claim, as
 * internal/claims.Match does: a plain path covers itself and everything
 * under it, a glob is matched segment by segment (`*` stays in its segment,
 * `**` spans any number of them).
 */
export function matchClaim(glob: string, p: string): boolean {
  glob = clean(glob)
  p = clean(p)
  if (!hasMeta(glob)) {
    return p === glob || p.startsWith(`${glob}/`)
  }
  const segs = p.split('/')
  return expandBraces(glob).some(g => matchSegs(g.split('/'), segs))
}

/** claimed reports whether any of globs covers p. */
export function claimed(globs: readonly string[], p: string): boolean {
  return globs.some(g => matchClaim(g, p))
}

/**
 * inWorktree returns p relative to root, or null when it lies outside it
 * (the scratchpad, another worktree: saddle's own hook rules on those). A
 * relative p is taken from root.
 */
export function inWorktree(root: string, p: string): string | null {
  const r = normalize(root)
  const abs = normalize(p.startsWith('/') ? p : `${r}/${p}`)
  if (abs === r || !abs.startsWith(`${r}/`)) {
    return null
  }
  const rel = abs.slice(r.length + 1)
  return rel === '.git' || rel.startsWith('.git/') ? null : rel
}

/** words splits one shell line into words and operators, quotes removed. */
function words(line: string): string[] {
  const out: string[] = []
  let cur = ''
  let isWord = false
  const flush = () => {
    if (isWord) {
      out.push(cur)
    }
    cur = ''
    isWord = false
  }
  for (let i = 0; i < line.length; i++) {
    const c = line[i] ?? ''
    if (c === "'" || c === '"') {
      const end = line.indexOf(c, i + 1)
      if (end < 0) {
        return [] // unbalanced: not worth guessing
      }
      cur += line.slice(i + 1, end)
      isWord = true
      i = end
    } else if (c === '\\' && i + 1 < line.length) {
      cur += line[++i]
      isWord = true
    } else if (/\s/.test(c)) {
      flush()
    } else if (';&|\n'.includes(c)) {
      flush()
      const two = line.slice(i, i + 2)
      out.push(two === '&&' || two === '||' ? two : c)
      i += two === '&&' || two === '||' ? 1 : 0
    } else if (c === '>') {
      // A redirect keeps its fd digits (2>) and its target stays the next word.
      const fd = /^(\d|&)$/.test(cur) ? cur : ''
      if (!fd) {
        flush()
      } else {
        cur = ''
        isWord = false
      }
      const isAppend = line[i + 1] === '>'
      const isClobber = line[i + 1] === '|'
      i += isAppend || isClobber ? 1 : 0
      if (line[i + 1] === '&') {
        // >&2: a dup onto another fd, no file.
        out.push('>&')
        i++
        continue
      }
      out.push('>')
    } else {
      cur += c
      isWord = true
    }
  }
  flush()
  return out
}

const OPERATORS = new Set([';', '&', '|', '&&', '||', '\n'])

/** args are a command's words after its name that are not options. */
const args = (w: string[]) => w.slice(1).filter(a => !a.startsWith('-'))

/**
 * bashTargets lists the paths a shell command plainly writes: redirect
 * targets, and the files of tee, touch, rm, cp, mv, install and sed -i. It
 * is a best effort, not a parser: it stops at a cd (later paths are relative
 * to somewhere else), skips any word holding $, a backtick or a glob, and
 * returns nothing for a line it cannot split.
 */
export function bashTargets(command: string): string[] {
  const out: string[] = []
  let cmd: string[] = []
  const flushCmd = () => {
    const name = cmd[0] ?? ''
    switch (name) {
      case 'tee':
      case 'touch':
      case 'rm':
        out.push(...args(cmd))
        break
      case 'cp':
      case 'mv':
      case 'install': {
        const a = args(cmd)
        if (a.length >= 2) {
          out.push(a[a.length - 1] ?? '')
        }
        break
      }
      case 'sed': {
        if (!cmd.some(a => a === '-i' || a.startsWith('-i') || a === '--in-place' || a.startsWith('--in-place='))) {
          break
        }
        const rest: string[] = []
        let hasScript = false
        for (let i = 1; i < cmd.length; i++) {
          const a = cmd[i] ?? ''
          if (a === '-e' || a === '-f' || a === '--expression' || a === '--file') {
            hasScript = true
            i++
          } else if (!a.startsWith('-')) {
            rest.push(a)
          }
        }
        out.push(...(hasScript ? rest : rest.slice(1)))
        break
      }
    }
    cmd = []
  }
  const w = words(command)
  for (let i = 0; i < w.length; i++) {
    const t = w[i] ?? ''
    if (OPERATORS.has(t)) {
      flushCmd()
      continue
    }
    if (t === '>&') {
      i++
      continue
    }
    if (t === '>') {
      const target = w[++i]
      if (target !== undefined && !OPERATORS.has(target)) {
        out.push(target)
      }
      continue
    }
    if (cmd.length === 0 && (t === 'cd' || t === 'pushd')) {
      break
    }
    cmd.push(t)
  }
  flushCmd()
  return out.filter(p => p !== '' && !p.startsWith('/dev/') && !/[$`*?[{]/.test(p))
}

/** heldMessage is what the worker reads when the guard holds its call. */
export function heldMessage(task: string, paths: readonly string[], globs: readonly string[]): string {
  const list = paths.join(', ')
  const has = globs.length > 0 ? globs.join(', ') : 'none'
  return (
    `[saddle] claim guard: ${list} ${paths.length === 1 ? 'is' : 'are'} outside ${task}'s claims (${has}). ` +
    `Claim first with the saddle claim tool (claim ${JSON.stringify(paths)}, or a glob that covers them), then retry the call. ` +
    `If the claim is refused, the path is another task's: ask its owner (ask_owner) instead. ` +
    `If you cannot claim it for any other reason, retry the same call unchanged: the guard holds a path only once, ` +
    `and the person can let it through with Proceed.`
  )
}

/** The guard's reading of the task: its claims and its worktree. */
type Claims = { globs: string[]; root: string }

/**
 * readClaims runs `saddle status --json` and returns task's claims, or null
 * when anything about it fails: the guard then lets the call through.
 */
async function readClaims($: EngineInterface, cwd: string, task: string): Promise<Claims | null> {
  try {
    const r = await $.process.run(['saddle', 'status', '--json'], { cwd, timeoutMs: 10000 })
    if (r.exitCode !== 0) {
      return null
    }
    const status = JSON.parse(r.stdout) as { tasks?: { id: string; claims?: string[]; worktree?: string }[] }
    const t = status.tasks?.find(x => x.id === task)
    if (!t) {
      return null
    }
    return { globs: t.claims ?? [], root: t.worktree || cwd }
  } catch {
    return null
  }
}

/** The paths a tool call writes, as the call names them. */
function written(e: { tool: string } & Record<string, unknown>): string[] {
  switch (e.tool) {
    case 'Edit':
    case 'Write':
      return typeof e.file_path === 'string' ? [e.file_path] : []
    case 'NotebookEdit':
      return typeof e.notebook_path === 'string' ? [e.notebook_path] : []
    case 'Bash':
      return typeof e.command === 'string' ? bashTargets(e.command) : []
    default:
      return []
  }
}

/** The tools the guard watches (spelled out in its matcher, so validate lists them). */
export const TOOLS = ['Edit', 'Write', 'NotebookEdit', 'Bash'] as const

// The last claims read, kept until a call falls outside them: a claim the
// worker just made is then read before the call is held. A reload starts
// it over; the holds themselves live in $.state.
let cache: Claims | null = null

/** outside lists the paths among paths the task neither claimed nor had held once. */
async function outside($: EngineInterface, c: Claims, paths: readonly string[]): Promise<string[]> {
  const { held } = await read($, guard)
  const rels = paths.map(p => inWorktree(c.root, p)).filter((p): p is string => p !== null)
  return [...new Set(rels)].filter(p => !held.includes(p) && !claimed(c.globs, p))
}

/** check returns why a call is held, or null to let it through. */
async function check($: EngineInterface, e: { tool: string } & Record<string, unknown>): Promise<string | null> {
  const task = await $.env.get('SADDLE_TASK')
  if (!task) {
    return null
  }
  const paths = written(e)
  if (paths.length === 0) {
    return null
  }
  if (cache && (await outside($, cache, paths)).length === 0) {
    return null
  }
  cache = await readClaims($, await $.session.cwd(), task)
  if (!cache) {
    return null // fail open
  }
  const hold = await outside($, cache, paths)
  if (hold.length === 0) {
    return null
  }
  await update($, guard, g => ({
    held: [...new Set([...g.held, ...hold])],
    pending: [...g.pending.filter(p => !hold.includes(p.path)), ...hold.map(path => ({ path, tool: e.tool }))],
  }))
  return heldMessage(task, hold, cache.globs)
}

/** dismiss takes a hold off the band. */
async function dismiss($: EngineInterface, path: string) {
  await update($, guard, g => ({ ...g, pending: g.pending.filter(p => p.path !== path) }))
}

/** proceed takes a hold off the band and tells the worker it may go ahead. */
async function proceed($: EngineInterface, path: string) {
  await dismiss($, path)
  await $.prompt.submit({
    text: `[saddle] The person let ${path} through the claim guard without a claim. Retry the call you were making on it.`,
  })
}

/**
 * registerClaimGuard adds the guard's hooks when the plugin's `claimGuard`
 * option is on. They act only in a worker session (SADDLE_TASK is set); the
 * orchestrator's session is left alone.
 */
export function registerClaimGuard(on: On, options: PluginOptions) {
  if (options[OPTION] !== true) {
    return
  }
  on('tool.call', { tool: ['Edit', 'Write', 'NotebookEdit', 'Bash'] }, async ($, e, next) => {
    const why = await check($, e)
    return why ? { deny: why } : next(e)
  })

  // The band lists the calls the guard held, each with Proceed (tell the
  // worker it may go ahead) and Dismiss. It draws nothing when none wait.
  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const { pending } = await read($, guard)
    if (e.props.hasSurvey || pending.length === 0) {
      return next(e)
    }
    const { Box, Text, Button } = $.ui.resolve(e)
    return (
      <Box key="claimguard" flexDirection="column">
        {pending.map(p => (
          <Box key={`held-${p.path}`} flexDirection="row" gap={1}>
            <Box key={`held-text-${p.path}`}>
              <Text color="warning" wrap="truncate-end">
                {`⏸ claim guard held ${p.tool} on ${p.path} (unclaimed)`}
              </Text>
            </Box>
            <Button key={`proceed-${p.path}`} label="Proceed" onPress={() => proceed($, p.path)} />
            <Button key={`dismiss-${p.path}`} label="Dismiss" role="dismiss" dimColor onPress={() => dismiss($, p.path)} />
          </Box>
        ))}
      </Box>
    )
  })
}
