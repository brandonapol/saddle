package bad

import (
	legacy "io/ioutil"
	"os"
)

// Bad writes state beside the database.
func Bad() {
	_, _ = os.ReadFile("fine")
	_ = os.WriteFile("automerge.json", nil, 0o600)
	_ = os.MkdirAll("deps", 0o700)
	_ = legacy.WriteFile("x", nil, 0o600)
}
