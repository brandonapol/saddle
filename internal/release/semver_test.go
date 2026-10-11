package release

import "testing"

func TestParseSemver(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Semver
		ok   bool
	}{
		{"v0.1.0", Semver{Minor: 1}, true},
		{"1.2.3", Semver{Major: 1, Minor: 2, Patch: 3}, true},
		{"v1.2.3-rc.1", Semver{Major: 1, Minor: 2, Patch: 3, Pre: "rc.1"}, true},
		{"v0.1.0-3-gabc1234", Semver{Minor: 1, Ahead: true}, true},
		{"v0.1.0-3-gabc1234-dirty", Semver{Minor: 1, Ahead: true}, true},
		{"abc1234", Semver{}, false},
		{"dev", Semver{}, false},
		{"v1.2", Semver{}, false},
	} {
		got, ok := ParseSemver(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseSemver(%q) = %+v, %v; want %+v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCompareSemver(t *testing.T) {
	order := []string{"v0.1.0-rc.1", "v0.1.0", "v0.1.0-2-gabc", "v0.1.1", "v0.2.0", "v1.0.0"}
	for i := range order {
		for j := range order {
			a, _ := ParseSemver(order[i])
			b, _ := ParseSemver(order[j])
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := Compare(a, b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
}

func TestValidTag(t *testing.T) {
	for in, want := range map[string]bool{"v0.1.0": true, "v10.20.30": true, "0.1.0": false, "v0.1": false, "v0.1.0-rc.1": false, "v01.0.0": false} {
		if got := ValidTag(in); got != want {
			t.Errorf("ValidTag(%q) = %v, want %v", in, got, want)
		}
	}
}
