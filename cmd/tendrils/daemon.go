package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/config"
)

func newDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run the sync engine (watch, upload, publish, reconcile)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, hasKey, err := config.LoadKey()
			if err != nil {
				return err
			}
			if !hasKey {
				return fmt.Errorf("not enrolled: run 'tendrils keygen' then 'tendrils enroll'")
			}
			cfg, _, err := config.Load()
			if err != nil {
				return err
			}
			if cfg.SyncRoot == "" {
				return fmt.Errorf("no sync root set: run 'tendrils enroll --root <folder>'")
			}

			npub, _ := id.Npub()
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "Enrolled and ready:")
			fmt.Fprintln(out, "  Identity: ", npub)
			fmt.Fprintln(out, "  Sync root:", cfg.SyncRoot)
			fmt.Fprintln(out)
			// The engine that drives the sync loop — watch → settle → encrypt →
			// upload blob → publish event, plus periodic reconcile against the
			// relay — is the next milestone. The pure core it will orchestrate
			// (reconcile, crypt, index, scan, event codec) is built and tested.
			return fmt.Errorf("sync engine not yet implemented (next milestone: internal/engine + Blossom client)")
		},
	}
}
