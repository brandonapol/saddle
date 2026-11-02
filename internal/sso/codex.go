package sso

import (
	"fmt"
	"strconv"
	"strings"
)

// Codex auth command timing. Codex re-runs the command every
// CodexRefreshMS and after a 401; a cached token costs one file read, so
// a short interval keeps tokens fresh. The timeout leaves room for a
// refresh round trip.
const (
	CodexTimeoutMS = 15000
	CodexRefreshMS = 60000
)

// CodexArgs are the codex -c overrides that make model provider id get its
// bearer token from `<bin> sso token --root <root> <name>` (#758). Codex
// 0.154+ reads [model_providers.<id>.auth]; it can't be combined with
// env_key or requires_openai_auth on the same provider. With baseURL set the
// provider is defined here too; otherwise the user's codex config.toml
// defines it.
func CodexArgs(bin, root, name, id, baseURL string) []string {
	p := "model_providers." + tomlKey(id)
	args := []string{"-c", "model_provider=" + strconv.Quote(id)}
	if baseURL != "" {
		args = append(args,
			"-c", p+".name="+strconv.Quote(id),
			"-c", p+".base_url="+strconv.Quote(baseURL))
	}
	cmdArgs := []string{"sso", "token", "--root", root, name}
	q := make([]string, len(cmdArgs))
	for i, a := range cmdArgs {
		q[i] = strconv.Quote(a)
	}
	return append(args,
		"-c", p+".auth.command="+strconv.Quote(bin),
		"-c", p+".auth.args=["+strings.Join(q, ",")+"]",
		"-c", fmt.Sprintf("%s.auth.timeout_ms=%d", p, CodexTimeoutMS),
		"-c", fmt.Sprintf("%s.auth.refresh_interval_ms=%d", p, CodexRefreshMS),
	)
}

// tomlKey quotes a dotted-path segment unless it is a bare key.
func tomlKey(k string) string {
	for _, r := range k {
		bare := r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !bare {
			return strconv.Quote(k)
		}
	}
	return k
}
