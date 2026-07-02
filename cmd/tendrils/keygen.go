package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/keys"
)

func newKeygenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "keygen",
		Short: "Generate a new Nostr identity for your sync set",
		Long: "Generate a fresh Nostr keypair. This one key is your whole identity: " +
			"it enrolls every device and decrypts your files.\n\n" +
			"Back up the nsec. It is the master key — if you lose it, your encrypted " +
			"blobs are unrecoverable; if it leaks, your files are readable.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := keys.Generate()
			if err != nil {
				return err
			}
			nsec, err := id.Nsec()
			if err != nil {
				return err
			}
			npub, err := id.Npub()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "New Tendrils identity generated.")
			fmt.Fprintln(out)
			fmt.Fprintln(out, "  Secret (BACK THIS UP):", nsec)
			fmt.Fprintln(out, "  Public:               ", npub)
			fmt.Fprintln(out)
			fmt.Fprintln(out, "Enroll a device with:  tendrils enroll --key", nsec, "--root <folder>")
			return nil
		},
	}
}
