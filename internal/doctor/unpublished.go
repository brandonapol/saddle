package doctor

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
)

// CheckUnpublished flags landed tasks with no PR (#819): their work is only
// on saddle's local train, so it is not on base and not done.
const CheckUnpublished = "landed without a PR"

const unpublishedAbout = "landed tasks whose PR isn't open: land and the sentinel open them, since work counts as done only once its PR merges into base"

// UnpublishedEnv is an Env that can list landed tasks without a PR. The
// check is skipped for envs without it.
type UnpublishedEnv interface {
	Unpublished() ([]string, error)
}

func unpublishedCheck(ue UnpublishedEnv) Result {
	ids, err := ue.Unpublished()
	if err != nil {
		return warn(CheckUnpublished, "could not read: "+err.Error(), "")
	}
	if len(ids) == 0 {
		return ok(CheckUnpublished, "every landed task has a PR")
	}
	return warn(CheckUnpublished,
		strings.Join(ids, ", ")+" landed but have no PR, so their work is not on base and not done",
		"saddle opens them on the next land or sentinel cycle; run `saddle prs` to open them now and see what blocks them")
}

// Unpublished lists the landed tasks without a PR. An uninitialized repo
// has none.
func (s system) Unpublished() ([]string, error) {
	if _, err := os.Stat(filepath.Join(string(s), ".saddle", "state.db")); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	a, err := app.Open(string(s))
	if err != nil {
		return nil, err
	}
	defer a.Close()
	return a.Unpublished()
}
