package cli

import (
	"fmt"
	"io"

	"github.com/brandonapol/saddle/internal/app"
)

// writeForecast prints the predicted collisions between live tasks, if any
// pair scores medium or above.
func writeForecast(out io.Writer, f *app.ForecastView) {
	if f == nil {
		return
	}
	for _, p := range f.Pairs {
		fmt.Fprintf(out, "collision %s: %s and %s: %s\n", p.Level, p.A, p.B, p.Reason)
	}
	if f.More > 0 {
		fmt.Fprintf(out, "collision: +%d more pairs at medium or above\n", f.More)
	}
	fmt.Fprintf(out, "collision: %d other pairs low or none (safe side by side)\n", f.Safe)
}
