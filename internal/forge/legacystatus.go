package forge

import "slices"

// legacyBaseStatus is the commit status saddle posted on every PR before
// ADR 0012. Open PRs still carry it at their heads.
const legacyBaseStatus = "saddle/base"

// legacyStatus reports whether c is saddle's legacy base status. A GitLab
// commit status reads as a job named after it, so its name counts too.
func legacyStatus(c CheckRun) bool {
	return c.Context == legacyBaseStatus || (c.Context == "" && c.Name == legacyBaseStatus)
}

// withoutLegacy drops saddle's legacy base status from cs: legacy status
// posted by saddle before ADR 0012 (#931); ignore until #938 deletes ci-red.
func withoutLegacy(cs []CheckRun) []CheckRun {
	return slices.DeleteFunc(cs, legacyStatus)
}

// ciWithoutLegacy is withoutLegacy for `checks` reads: legacy status posted
// by saddle before ADR 0012 (#931); ignore until #938 deletes ci-red.
func ciWithoutLegacy(cs []CICheck) []CICheck {
	return slices.DeleteFunc(cs, func(c CICheck) bool { return c.Name == legacyBaseStatus })
}
