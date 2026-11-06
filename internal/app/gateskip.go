package app

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/lintgate"
)

// gatedBefore reports whether the train may skip running cmd on dir's
// rebased tree because a gate already passed it (#800). Both have to hold:
//
//   - the tree is provably the one that passed: dir is clean, its HEAD's
//     tree is pre's (the branch before the train rebased it), and pre
//     already sat on base, the integration it lands on, so the rebase only
//     replayed the same commits onto the same base;
//   - a gate stamped that tree green for cmd: the train's own test stamp,
//     done's lint stamp or the repo hook's pre-commit stamp when the lint
//     gate is the same command, or the pre-publish gate's record.
//
// Anything else (a moved integration, regenerated files, an edit, another
// command) runs the gate.
func (a *App) gatedBefore(dir, base, pre, cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" || cmd == NoTestCmd || base == "" || pre == "" {
		return false
	}
	tree := cleanTree(dir)
	if tree == "" {
		return false
	}
	if pt, err := gitx.Run(dir, "rev-parse", pre+"^{tree}"); err != nil || pt != tree {
		return false
	}
	if _, err := gitx.Run(dir, "merge-base", "--is-ancestor", base, pre); err != nil {
		return false
	}
	return a.treePassed(dir, tree, cmd)
}

// treePassed reports whether some gate recorded tree green for cmd.
func (a *App) treePassed(dir, tree, cmd string) bool {
	state := hooksState(dir)
	if state != "" && stamped(filepath.Join(state, lintStamp), stampLine(tree, testGate(cmd))) {
		return true
	}
	if g := a.LintGate(); g.Cmd != "" && lintgate.SameCmd(g.Cmd, cmd) && passedLint(state, tree, g) {
		return true
	}
	s, err := a.Gate()
	if err != nil {
		return false
	}
	at, ok := s.Passed[tree+" "+cmd]
	return ok && time.Since(at) < gatePassedTTL
}

// stampTest records that tree in dir passed the test command cmd, so a
// retried land of the same tree on the same base doesn't run it again.
func stampTest(dir, cmd string) {
	if cmd = strings.TrimSpace(cmd); cmd == "" || cmd == NoTestCmd {
		return
	}
	stampLint(hooksState(dir), cleanTree(dir), testGate(cmd))
}

// testGate is the test command as a gate, for the pass stamps. A stamp
// keys on the command alone, so it is the same stamp lint would write for
// the same command.
func testGate(cmd string) lintgate.Gate {
	return lintgate.Gate{Kind: "config", Cmd: strings.TrimSpace(cmd)}
}
