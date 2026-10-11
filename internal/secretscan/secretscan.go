// Package secretscan finds credential-shaped literals in source, the same
// shapes PR secret scanners (GitGuardian) flag. Tests that need such strings
// must build them at runtime so no commit ever holds a whole one: scanners
// check every commit in a pull request, so one slip fails CI for the stack.
package secretscan

import "regexp"

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----\r?\n[A-Za-z0-9+/=\r\n]{64,}`),
	regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bsk-(?:ant-|proj-)[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
	regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), // JWT
}

// Find returns every credential-shaped literal in b.
func Find(b []byte) [][]byte {
	var out [][]byte
	for _, re := range patterns {
		out = append(out, re.FindAll(b, -1)...)
	}
	return out
}
