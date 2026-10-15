package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/forge"
)

// Forge is the repo's git host, as [forge] selects it (docs/adr/0001-forge-layer.md).
func (a *App) Forge() forge.Forge { return forge.New(a.Cfg.Forge, a.Root) }

// gh runs the gh CLI in dir and returns its trimmed stdout. It is for
// gh-stack, the GitHub-only extension behind ghStackRun.
func gh(dir string, args ...string) (string, error) {
	out, errb, err := forge.Output(context.Background(), dir, forge.GH, args...)
	if err != nil {
		return "", fmt.Errorf("gh %s: %w: %s", strings.Join(args[:2], " "), err, strings.TrimSpace(errb))
	}
	return strings.TrimSpace(out), nil
}

// isParseErr reports whether err is a forge's answer that didn't parse,
// rather than a failed call.
func isParseErr(err error) bool {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	return errors.As(err, &se) || errors.As(err, &te)
}
