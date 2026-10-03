// Package banner prints saddle's "howdy" greeting: a small ASCII banner with
// a bucking horse, shown by saddle init and the plugin's first use in a repo.
package banner

import (
	"fmt"
	"io"
	"os"
)

const howdy = `   _                      _                         ,--,
  | |_   ___ __ __ __  __| | _  _             _ ___/ /\|
  | ' \ / _ \\ V  V / / _` + "`" + ` || || |        ,;'( )__, )  ~
  |_||_|\___/ \_/\_/  \__,_| \_, |       //  //   '--;
                             |__/        '   \     | ^
                                              ^    ^

  Howdy, partner! Saddle up: your herd of agents is ready to ride.
`

// Howdy returns the plain banner: no color, at most 12 lines of 80 columns.
func Howdy() string { return howdy }

// Options controls Print.
type Options struct {
	// Quiet suppresses the banner (--quiet).
	Quiet bool
	// IsTTY reports whether w is a terminal. Nil checks for a character
	// device.
	IsTTY func(io.Writer) bool
}

// Print writes the banner to w unless opts.Quiet or w is not a terminal. It
// is colored unless NO_COLOR is set (https://no-color.org).
func Print(w io.Writer, opts Options) {
	if opts.Quiet {
		return
	}
	isTTY := opts.IsTTY
	if isTTY == nil {
		isTTY = IsTerminal
	}
	if !isTTY(w) {
		return
	}
	if os.Getenv("NO_COLOR") == "" {
		fmt.Fprint(w, "\x1b[33m"+howdy+"\x1b[0m")
		return
	}
	fmt.Fprint(w, howdy)
}

// IsTerminal reports whether w is a file attached to a terminal.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
