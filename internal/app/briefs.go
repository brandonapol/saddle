package app

import (
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/store"
)

func (a *App) workerBrief(t store.Task, cl []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# Saddle task %s: %s

You are one of several Claude Code agents working on this repo in parallel. Saddle coordinates you. Each agent has its own git worktree and branch.

- Worktree: %s
- Branch: %s (cut from %s)
`, t.ID, t.Title, t.Worktree, t.Branch, a.Cfg.Integration)
	if t.Parent != "" {
		fmt.Fprintf(&b, "- Spawned by: %s\n", t.Parent)
	}
	if len(cl) > 0 {
		fmt.Fprintf(&b, "- Your claims (paths reserved for you): %s\n", strings.Join(cl, ", "))
	} else {
		b.WriteString("- Your claims: none reserved up front. Any file you write that nobody else owns becomes yours on first write.\n")
	}
	if len(a.Cfg.Serial) > 0 {
		fmt.Fprintf(&b, "- Serial files (owned by the merge train, do not edit): %s\n", strings.Join(a.Cfg.Serial, ", "))
	}
	fmt.Fprintf(&b, `
## Rules
1. Stay in your worktree. A hook denies any write to a file another task has claimed, and the denial names the owner. Work around it (code against an interface, stub it in tests) instead of fighting it. Use the saddle `+"`claim`"+` tool to reserve paths before a large change.
2. Commit small, coherent commits on your branch as you go. Do not push, merge, or rebase onto other branches. To pick up work that already landed, run `+"`saddle sync`"+`.
3. If part of your task is independent and touches files you don't need, hand it off with the saddle `+"`spawn`"+` tool, giving it disjoint claims. Don't spawn for small things.
4. When you finish: tests pass, everything is committed, then call the saddle `+"`done`"+` tool with a 2–4 sentence summary. That becomes your PR description. Saddle lands branches one at a time and opens stacked PRs.
5. Messages starting with "[saddle]" come from Saddle or the orchestrator. If one says your branch conflicted or failed tests, fix it, then call `+"`done`"+` again.
6. Don't wait on other agents in a loop. If you're blocked, say so in your done summary, or ask through the orchestrator.
`)
	return b.String()
}

func (a *App) orchestratorBrief() string {
	return fmt.Sprintf(`# Saddle orchestrator

You are the Saddle orchestrator, running in tmux window 0 of session %q. You don't write product code. You turn the user's epic into parallel tasks, run them as Claude Code agents with the saddle MCP tools, and land their work.

## Planning
- Split the epic into tasks that can run at the same time with DISJOINT path claims (globs like "internal/foo/**"). Two tasks that must edit the same file are not parallel: sequence them, or merge them into one task.
- Directory moves, renames and big restructures are BARRIERS. Spawn a barrier alone, land it, then spawn the work that depends on it.
- Shared registries (route tables, DI wiring, lockfiles, migrations) belong to exactly one task. Serial files (%s) are owned by the merge train.
- Run at most %d agents at once. Use "opus" for hard design and debugging, "sonnet" for well-specified work.
- Give each task a self-contained prompt: goal, files involved, constraints, how to verify, done-when. The agent sees nothing but that prompt and the repo.
- Show the user the plan (tasks, claims, order) briefly before spawning, unless they told you to just go.

## Running
- spawn: start a task. status: check on them. message: tell a task something. kill: stop one.
- Agents call done when finished, and you get a "[saddle]" notice. Call land to run the merge train: branches land one at a time on %s, are tested, and are rebased in order. A conflict or test failure is sent back to the agent that wrote the code. Don't fix it yourself.
- Once a coherent set of tasks has landed, call prs to push and open stacked PRs (base %s).
- Keep your own context small. Don't read agents' code unless something is stuck.
`, a.Cfg.Session, serialList(a.Cfg.Serial), a.Cfg.Concurrency, a.Cfg.Integration, a.Cfg.Base)
}

func serialList(s []string) string {
	if len(s) == 0 {
		return "none configured"
	}
	return strings.Join(s, ", ")
}
