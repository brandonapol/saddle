// Package server maps paths to handlers through one shared route table.
package server

// Route binds a path to a handler.
type Route struct {
	Path    string
	Handler func() string
}

// Routes is every route the server serves. New routes are added here.
var Routes = []Route{
	{"/hello", hello},
	// routes end
}

// Serve runs the handler for path, or reports it missing.
func Serve(path string) (string, bool) {
	for _, r := range Routes {
		if r.Path == path {
			return r.Handler(), true
		}
	}
	return "", false
}
