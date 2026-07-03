package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/config"
	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/scan"
	"ca.punkscience.tendrils/internal/tree"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report what Tendrils is doing and has done",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			id, hasKey, err := config.LoadKey()
			if err != nil {
				return err
			}
			if !hasKey {
				fmt.Fprintln(out, "Not enrolled. Run 'tendrils keygen' then 'tendrils enroll'.")
				return nil
			}
			cfg, _, err := config.Load()
			if err != nil {
				return err
			}

			npub, _ := id.Npub()
			fmt.Fprintln(out, "Identity: ", npub)
			fmt.Fprintln(out, "Sync root:", orNone(cfg.SyncRoot))
			fmt.Fprintln(out, "Relays:   ", orDiscovery(cfg.Relays))
			fmt.Fprintln(out, "Storage:  ", orDiscovery(cfg.BlossomServers))

			last, pending, conflicts, err := localStatus(cfg.SyncRoot)
			if err != nil {
				return err
			}
			fmt.Fprintln(out, "Last reconcile:", formatTime(last))
			fmt.Fprintln(out, "Pending changes:", pending)
			fmt.Fprintln(out, "Conflicts:     ", conflicts)
			fmt.Fprintln(out, "Reachability:   not checked (run 'tendrils daemon' to connect)")
			return nil
		},
	}
}

// localStatus derives what can be known without a running daemon: the last
// recorded reconcile, how many local files differ from the last-synced base
// (pending upload/delete), and how many conflict copies await the owner.
func localStatus(root string) (last time.Time, pending, conflicts int, err error) {
	idxPath, err := config.IndexPath()
	if err != nil {
		return
	}
	store, err := index.OpenReadOnly(idxPath)
	if err != nil {
		return
	}
	if store == nil {
		// Database does not exist yet — no syncs have run.
		return
	}
	defer store.Close()

	last, err = store.LastReconcile()
	if err != nil {
		return
	}
	base, err := store.All()
	if err != nil {
		return
	}
	if root == "" {
		return
	}
	local, err := scan.Tree(root)
	if err != nil {
		return
	}

	for path, e := range local {
		if scan.IsConflictCopy(path) {
			conflicts++
			continue
		}
		if b := base[path]; !tree.SameContent(b, e) {
			pending++ // new or edited since last sync
		}
	}
	for path, b := range base {
		if !b.Live() {
			continue
		}
		if _, stillHere := local[path]; !stillHere {
			pending++ // deleted locally since last sync
		}
	}
	return
}

func orNone(s string) string {
	if s == "" {
		return "(not set)"
	}
	return s
}

func orDiscovery(ss []string) string {
	if len(ss) == 0 {
		return "(discovered from key)"
	}
	return fmt.Sprint(ss)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}
