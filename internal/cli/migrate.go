package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/brandonapol/saddle/internal/store"
	"github.com/spf13/cobra"
)

func migrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending state.db migrations now, backing the database up first",
		Long: `Opens this repo's .saddle/state.db with this binary, which applies any
migrations it has that the database lacks. A database an older saddle wrote is
first copied to state.db.bak-schema<N>-<time>. Every other command migrates
the same way on first use.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := currentSaddleRoot()
			if root == "" {
				return errors.New("not in a saddle repo (run saddle init)")
			}
			path := filepath.Join(root, ".saddle", "state.db")
			before, err := store.FileSchemaVersion(path)
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(cmd.OutOrStdout(), "No state.db yet; nothing to migrate.")
				return nil
			}
			if err != nil {
				return err
			}
			want := store.SchemaVersion()
			if before > want {
				fmt.Fprintf(cmd.OutOrStdout(), "state.db is at schema %d, newer than this binary's %d: a newer saddle wrote it. Left as is.\n", before, want)
				return nil
			}
			if before == want {
				fmt.Fprintf(cmd.OutOrStdout(), "state.db is at schema %d; nothing to migrate.\n", want)
				return nil
			}
			s, err := store.Open(path)
			if err != nil {
				return err
			}
			if err := s.Close(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Migrated state.db schema %d -> %d.\n", before, want)
			if bs, _ := filepath.Glob(fmt.Sprintf("%s.bak-schema%d-*", path, before)); len(bs) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Backup: %s\n", bs[len(bs)-1])
			}
			return nil
		},
	}
}
