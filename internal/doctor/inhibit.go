package doctor

import (
	"runtime"

	"github.com/brandonapol/saddle/internal/orchrc/inhibit"
)

// CheckInhibit reports whether saddle can keep the machine awake while
// autopilot runs (#409).
const CheckInhibit = "sleep inhibitor"

func (r *run) inhibitor() Result { return inhibitCheck(runtime.GOOS, r.env.LookPath) }

// inhibitCheck warns when goos has no sleep inhibitor saddle can hold:
// autopilot and Remote Control stall when the machine sleeps.
func inhibitCheck(goos string, lookPath func(string) (string, error)) Result {
	var res Result
	if t, found := inhibit.For(goos, lookPath); found {
		res = ok(CheckInhibit, t.Name+" (held while autopilot runs)")
	} else {
		res = warn(CheckInhibit, "none found: the machine may sleep while autopilot or saddle up --rc runs", inhibit.Hint(goos))
	}
	res.About = "whether saddle can keep the machine awake while autopilot runs, which Remote Control needs"
	return res
}
