package codec

import "testing"

func TestGeneratedMatches(t *testing.T) {
	if len(Spec) != len(Generated) {
		t.Fatalf("Spec %v, Generated %v", Spec, Generated)
	}
	for i := range Spec {
		if Spec[i] != Generated[i] {
			t.Fatalf("Spec %v, Generated %v", Spec, Generated)
		}
	}
}
