package prgraph

import "testing"

// A gate or rebase built without an onto (a host's record of one it ran
// itself) prints and keys without panicking.
func TestActionWithoutOnto(t *testing.T) {
	for _, k := range []ActionKind{ActGate, ActRebase} {
		a := Action{Kind: k, Change: "c"}
		if got := a.String(); got != string(k)+"(c, @+)" {
			t.Errorf("String = %q", got)
		}
		if got := a.AttemptKey(); got != string(k)+"/c//+" {
			t.Errorf("AttemptKey = %q", got)
		}
	}
}
