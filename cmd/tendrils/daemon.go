package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/blob"
	"ca.punkscience.tendrils/internal/config"
	"ca.punkscience.tendrils/internal/engine"
	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/relay"
)

func newDaemonCmd() *cobra.Command {
	var interval time.Duration

	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the sync engine (periodic reconcile against relay + Blossom)",
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
			// Config discovery (NIP-65 / kind-10063) is a later milestone; until it
			// exists the daemon needs relays and a Blossom server set explicitly.
			if len(cfg.Relays) == 0 {
				return fmt.Errorf("no relays configured: run 'tendrils enroll --relay wss://...' (discovery not yet implemented)")
			}
			if len(cfg.BlossomServers) == 0 {
				return fmt.Errorf("no Blossom server configured: run 'tendrils enroll --blossom https://...' (discovery not yet implemented)")
			}

			idxPath, err := config.IndexPath()
			if err != nil {
				return err
			}
			idx, err := index.Open(idxPath)
			if err != nil {
				return err
			}
			defer idx.Close()

			// Blossom multi-server mirroring is out of v1 scope; use the first.
			blobs := blob.New(cfg.BlossomServers[0], id)
			relays := relay.New(cfg.Relays)
			defer relays.Close()

			log := slog.New(slog.NewTextHandler(cmd.OutOrStdout(), nil))
			eng, err := engine.New(cfg.SyncRoot, id, idx, blobs, relays, log)
			if err != nil {
				return err
			}

			npub, _ := id.Npub()
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "Tendrils daemon starting:")
			fmt.Fprintln(out, "  Identity: ", npub)
			fmt.Fprintln(out, "  Sync root:", cfg.SyncRoot)
			fmt.Fprintln(out, "  Relays:   ", cfg.Relays)
			fmt.Fprintln(out, "  Storage:  ", cfg.BlossomServers[0])
			fmt.Fprintln(out, "  Interval: ", interval)
			fmt.Fprintln(out)

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			return runLoop(ctx, eng, interval, log)
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", time.Minute, "how often to reconcile against the relay")
	return cmd
}

// runLoop reconciles once immediately (startup catch-up / bootstrap) and then on
// the interval, until the context is cancelled (Ctrl-C). A failed pass is logged
// and the loop continues; pending changes stay queued in the working tree.
func runLoop(ctx context.Context, eng *engine.Engine, interval time.Duration, log *slog.Logger) error {
	sync := func() {
		if err := eng.Sync(ctx); err != nil {
			log.Error("reconcile pass failed", "err", err)
		} else {
			log.Info("reconcile pass complete")
		}
	}
	sync()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("daemon stopping")
			return nil
		case <-ticker.C:
			sync()
		}
	}
}
