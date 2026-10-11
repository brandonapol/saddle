package app

import (
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// BlockerKindOf must agree with the filer, so doctor --predict counts an
// escalation under the same class as its blocker issue (#408).
func TestBlockerKindOfAgreesWithFiler(t *testing.T) {
	t.Parallel()
	f, _ := blockerFiler(t)
	for ev, want := range blockerKinds {
		b, ok := f.App.blockerFromEvent(store.Event{Kind: ev, Data: "x|y"})
		if !ok || b.Kind != want {
			t.Errorf("%s: filer gives %q (%v), BlockerKindOf %q", ev, b.Kind, ok, want)
		}
	}
	if _, ok := BlockerKindOf("decision_open"); ok {
		t.Error("decision_open is not an escalation")
	}
}
