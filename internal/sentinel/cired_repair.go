package sentinel

import (
	"context"

	"github.com/brandonapol/saddle/internal/app"
)

// onRed decides what to do about a layer that newly went red and says what
// it did, for the notice.
func (c *CIRed) onRed(context.Context, app.CIRedLayer) string { return "" }
