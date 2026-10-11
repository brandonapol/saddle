package app

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// #274: each pre-publish check runs in the gate environment, its temp dirs
// under GateTmpdir like the train's land gate, not wherever TMPDIR points.
func TestGateLayerRunsInGateTmpdir(t *testing.T) {
	t.Parallel()
	a, _, ts := gateStack(t)
	dir, err := a.gateWorktree(0)
	must(t, err)
	head := git(t, a.Root, "rev-parse", a.Cfg.Integration)
	base := a.GateTmpdir()
	check := GateCheck{Name: "prepublish", Cmd: `case "$TMPDIR" in "` + base + `"/*) ;; *) echo "TMPDIR=$TMPDIR"; exit 1;; esac`}
	var mu sync.Mutex
	r, _, err := a.gateLayer(dir, gateJob{task: ts[0].ID, head: head}, []GateCheck{check}, map[string]time.Time{}, &mu)
	must(t, err)
	if r != nil {
		t.Fatalf("the check's TMPDIR is not under %s:\n%s", base, r.out)
	}
}

// #274: publish's gate on a head that keeps failing on the environment
// refuses to push, but blames the environment, not the head, and marks
// nothing red.
func TestPublishGateEnvironmentFailureDoesNotBlameTheHead(t *testing.T) {
	t.Parallel()
	a, origin, _ := publishSetup(t)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	a.Cfg.Train.Prepublish.Cmd = "echo 'compile: writing output: disk quota exceeded'; exit 1"
	_, err := a.Publish(PublishReq{Target: "t1", BranchName: "fix/one"})
	if err == nil {
		t.Fatal("publish pushed a head whose check could not run")
	}
	for _, want := range []string{"pre-publish gate hit the environment", "disk quota exceeded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "fails its") {
		t.Errorf("error blames the head:\n%v", err)
	}
	if remoteRev(t, origin, "fix/one") != "" {
		t.Fatal("publish pushed the head")
	}
	if s, _ := a.Gate(); len(s.Red) != 0 {
		t.Fatalf("the environment marked a layer red: %+v", s.Red)
	}
}

// #274: a re-cut layer whose cheap check keeps failing on the environment
// is not flagged by restack: no red record, nothing held, no repair notice.
func TestGateRestackEnvironmentFailureFlagsNothing(t *testing.T) {
	t.Parallel()
	a, _, ts := gateStack(t)
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	a.Cfg.Train.Prepublish.Cmd = "echo 'sqlite: disk I/O error'; exit 1"
	t3, t4 := ts[2], ts[3]
	plan := []restacked{
		{landedTask: landedTask{Task: t3, To: "old3"}, NewFrom: git(t, a.Root, "rev-parse", a.Cfg.Integration+"~2"), NewTo: git(t, a.Root, "rev-parse", a.Cfg.Integration+"~1")},
		{landedTask: landedTask{Task: t4, To: git(t, a.Root, "rev-parse", a.Cfg.Integration)}, NewFrom: "x", NewTo: git(t, a.Root, "rev-parse", a.Cfg.Integration)},
	}
	rs, err := a.restackLayout(plan)
	must(t, err)
	reds, held := a.gateRestack(rs)
	if len(reds) != 0 || len(held) != 0 {
		t.Fatalf("restack blamed the layer for the environment: reds %+v, held %v", reds, held)
	}
	if s, _ := a.Gate(); len(s.Red) != 0 {
		t.Fatalf("the environment marked a layer red: %+v", s.Red)
	}
	ns, err := a.Store.PeekNotices(OrchestratorID, false)
	must(t, err)
	for _, n := range ns {
		if strings.Contains(n.Text, "Restack re-cut") {
			t.Fatalf("orchestrator told to repair a layer for the environment: %s", n.Text)
		}
	}
}
