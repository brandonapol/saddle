package cli

import "testing"

func TestPRsOffersPreviewPushAndGateModes(t *testing.T) {
	cmd:=prsCmd()
	for _, name:=range []string{"dry-run","push-only","gate-only"} {
		if cmd.Flags().Lookup(name)==nil {t.Errorf("prs missing --%s",name)}
	}
}
