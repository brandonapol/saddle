//go:build e2e

package e2e

import (
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// spellCheck stands in for cspell: a file using the word needs the file that
// adds it to the allowlist.
const spellCheck = `test ! -f gamma/uses-word.txt || test -f delta/allow-word.txt || { echo 'gamma/uses-word.txt:1: Unknown word (uploaders)'; exit 1; }`

// TestJourneyPrepublishGateHoldsRedLayer (#223): four tasks land as one
// stack. Layer 3 uses a word only layer 4 adds to the allowlist, so layer
// 3's own tip fails the spelling check while layer 4's tip, and integration,
// pass. prs publishes layers 1 and 2, holds 3 and 4, and the failure names
// layer 3, the check and its output. With the gate off, all four go out.
func TestJourneyPrepublishGateHoldsRedLayer(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\nprepublish.cmd = \"" + strings.ReplaceAll(spellCheck, `"`, `\"`) + "\"\n"})
	layers := []struct {
		id, title, dir, file string
	}{
		{"t1", "Alpha work", "alpha", "work.txt"},
		{"t2", "Beta work", "beta", "work.txt"},
		{"t3", "Use uploaders", "gamma", "uses-word.txt"},
		{"t4", "Allow uploaders", "delta", "allow-word.txt"},
	}
	for _, l := range layers {
		w.Spawn(l.id, l.title, []string{l.dir + "/**"},
			fa.Write(l.dir+"/"+l.file, l.id+"\n"), fa.Commit(l.title), fa.Done(l.title+"."))
		w.WaitTask(l.id, "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
		w.MustSaddle("land")
		if v := w.Task(l.id); v.Status != "landed" {
			t.Fatalf("%s didn't land: %+v", l.id, v)
		}
	}

	r := w.Saddle("prs")
	if r.Code == 0 {
		t.Fatalf("prs published a stack whose layer 3 is red at its own tip: %s", r)
	}
	for _, want := range []string{"layer t3", "prepublish check", "uses-word.txt", "Unknown word (uploaders)", "(t4)"} {
		if !strings.Contains(r.Stderr, want) {
			t.Errorf("prs error lacks %q:\n%s", want, r.Stderr)
		}
	}
	for _, l := range layers {
		published := w.Task(l.id).PR != ""
		pushed := w.OriginFile(w.Task(l.id).Branch, l.dir+"/"+l.file) != ""
		if want := l.id == "t1" || l.id == "t2"; published != want || pushed != want {
			t.Errorf("%s: PR %v, pushed %v; want published %v", l.id, published, pushed, want)
		}
	}
	if st := w.MustSaddle("status"); st.Code != 0 {
		t.Fatalf("status: %s", st)
	}

	// The escape hatch: prepublish.off publishes everything.
	w.WriteConfig(Options{Tables: "[train]\noutput = \"single\"\nprepublish.off = true\n"})
	w.MustSaddle("prs")
	for _, l := range layers {
		if w.Task(l.id).PR == "" {
			t.Errorf("%s has no PR with the gate off", l.id)
		}
	}
}
