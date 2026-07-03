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
	"ca.punkscience.tendrils/internal/keys"
	"ca.punkscience.tendrils/internal/relay"
	"ca.punkscience.tendrils/internal/serverlist"
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
			// Relays still can't be discovered (NIP-65 is a later milestone), but
			// the Blossom server can: a device enrolled with only --key + --relay
			// discovers where blobs live from the owner's kind-10063 list below.
			if len(cfg.Relays) == 0 {
				return fmt.Errorf("no relays configured: run 'tendrils enroll --relay wss://...'")
			}

			idxPath, err := config.IndexPath()
			if err != nil {
				return err
			}
			// Probe the path is usable before starting the loop; this surfaces
			// permission or path errors early rather than at the first sync tick.
			if idx, err := index.Open(idxPath); err != nil {
				return err
			} else {
				idx.Close()
			}

			relays := relay.New(cfg.Relays)
			defer relays.Close()

			log := slog.New(slog.NewTextHandler(cmd.OutOrStdout(), nil))

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			// Reconcile the Blossom server list with the owner's published one:
			// discover it when this device has none configured, otherwise publish
			// the union (minus loopback addresses) so keyless devices can find it.
			if err := resolveBlossom(ctx, &cfg, id, relays, log); err != nil {
				return err
			}

			// Blossom multi-server mirroring is out of v1 scope; use the first.
			blobs := blob.New(cfg.BlossomServers[0], id)

			npub, _ := id.Npub()
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "Tendrils daemon starting:")
			fmt.Fprintln(out, "  Identity: ", npub)
			fmt.Fprintln(out, "  Sync root:", cfg.SyncRoot)
			fmt.Fprintln(out, "  Relays:   ", cfg.Relays)
			fmt.Fprintln(out, "  Storage:  ", cfg.BlossomServers[0])
			fmt.Fprintln(out, "  Interval: ", interval)
			fmt.Fprintln(out)

			return runLoop(ctx, idxPath, cfg.SyncRoot, id, blobs, relays, interval, log)
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", time.Minute, "how often to reconcile against the relay")
	return cmd
}

// resolveBlossom settles which Blossom server(s) this device uses, using the
// owner's kind-10063 list as the shared source of truth:
//
//   - No server configured locally → discover it from the relay. This is what
//     lets a device enroll with only --key + --relay and still find the blobs.
//   - Server(s) configured locally → publish the union of the discovered list and
//     this device's own shareable servers, so keyless devices can discover it.
//     Loopback addresses are stripped: advertising 127.0.0.1 to the whole
//     identity would send a remote puller to its own empty box.
//
// Publishing is best-effort: a relay hiccup must not stop the device syncing
// against a server it already knows.
func resolveBlossom(ctx context.Context, cfg *config.Config, id *keys.Identity, relays *relay.Client, log *slog.Logger) error {
	discovered, err := relays.FetchServerList(ctx, id.PublicHex())
	if err != nil {
		log.Warn("could not fetch published Blossom server list", "err", err)
	}

	if len(cfg.BlossomServers) == 0 {
		if len(discovered) == 0 {
			return fmt.Errorf("no Blossom server configured and none discovered from the relay: run 'tendrils enroll --blossom https://...' on this device, or start the daemon on a device that has one so it can publish the list")
		}
		log.Info("discovered Blossom server(s) from the relay", "servers", discovered)
		cfg.BlossomServers = discovered
		return nil
	}

	union := serverlist.Merge(discovered, serverlist.Shareable(cfg.BlossomServers))
	if len(union) > 0 {
		evt, err := serverlist.Sign(union, id.SecretHex())
		if err != nil {
			log.Warn("could not sign Blossom server list", "err", err)
			return nil
		}
		if err := relays.Publish(ctx, evt); err != nil {
			log.Warn("could not publish Blossom server list", "err", err)
		} else {
			log.Info("published Blossom server list", "servers", union)
		}
	}
	return nil
}

// runLoop reconciles once immediately (startup catch-up / bootstrap) and then on
// the interval, until the context is cancelled (Ctrl-C). A failed pass is logged
// and the loop continues; pending changes stay queued in the working tree.
//
// The index database is opened at the start of each sync pass and closed when
// the pass completes, so that commands like "tendrils status" can read the index
// between passes without waiting on a file lock.
func runLoop(ctx context.Context, idxPath, root string, id *keys.Identity, blobs *blob.Client, relays *relay.Client, interval time.Duration, log *slog.Logger) error {
	sync := func() {
		idx, err := index.Open(idxPath)
		if err != nil {
			log.Error("open index", "err", err)
			return
		}
		defer idx.Close()

		eng, err := engine.New(root, id, idx, blobs, relays, log)
		if err != nil {
			log.Error("create engine", "err", err)
			return
		}
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
