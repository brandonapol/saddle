package cli

import (
	"fmt"
	"slices"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/doctor"
)

// orphanTaskCheck (#690) reports processes still tagged SADDLE_TASK with a
// task that already landed, was killed or finished with no change: leaks
// like the 16 `yes` that ran 31 hours.
func orphanTaskCheck(orphans map[string][]int) doctor.Result {
	const name = "orphaned task processes"
	if len(orphans) == 0 {
		return doctor.Result{Name: name, Status: doctor.OK, Detail: "none"}
	}
	var tasks []string
	n := 0
	for id, pids := range orphans {
		tasks = append(tasks, id)
		n += len(pids)
	}
	slices.Sort(tasks)
	return doctor.Result{Name: name, Status: doctor.Warn,
		Detail: fmt.Sprintf("%d process(es) still running for ended tasks %v", n, tasks),
		Fix:    "run saddle up: its sweep kills them"}
}

func runOrphanTaskCheck(a *app.App) doctor.Result {
	return orphanTaskCheck(a.OrphanTaskProcs())
}
