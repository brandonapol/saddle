package cli

import (
	"bytes"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

func TestWriteForecast(t *testing.T) {
	var b bytes.Buffer
	writeForecast(&b, nil)
	if b.Len() != 0 {
		t.Fatalf("nil forecast printed %q", b.String())
	}
	writeForecast(&b, &app.ForecastView{
		Pairs: []app.ForecastPair{{A: "t2", B: "t3", Level: "critical", Reason: "same symbol: a.go F (func)"}},
		More:  1,
		Safe:  4,
	})
	want := "collision critical: t2 and t3: same symbol: a.go F (func)\n" +
		"collision: +1 more pairs at medium or above\n" +
		"collision: 4 other pairs low or none (safe side by side)\n"
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
}
