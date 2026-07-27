package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"ca.punkscience.tendrils/internal/config"
	"ca.punkscience.tendrils/internal/index"
)

// newRetryCmd clears retry backoff so held-back paths are attempted on the next
// pass.
//
// Backoff deliberately waits a long time before retrying a failure it judged
// permanent — a day, for a file no server would accept. That is right while the
// cause persists and wrong the moment the operator fixes it: adding storage, or
// putting an uncapped server first, does not change any file, so nothing makes
// the engine reconsider and the tree sits visibly stuck for another day.
//
// This is the manual "I fixed it, try again" the schedule cannot infer.
func newRetryCmd() *cobra.Command {
	var list bool

	cmd := &cobra.Command{
		Use:   "retry",
		Short: "Clear retry backoff so stuck files are attempted on the next pass",
		Long: "Files that keep failing are held back on a growing schedule, up to a day for\n" +
			"failures nothing but a change of configuration can fix. After making such a\n" +
			"change, this clears the wait so the next reconcile tries them again.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			if _, running, _ := queryDaemon(); running {
				return fmt.Errorf("the daemon is running and holds the index; stop it first (systemctl --user stop tendrils-daemon)")
			}
			idxPath, err := config.IndexPath()
			if err != nil {
				return err
			}
			store, err := index.Open(idxPath)
			if err != nil {
				return err
			}
			defer store.Close()

			retries, err := store.Retries()
			if err != nil {
				return err
			}
			if len(retries) == 0 {
				fmt.Fprintln(out, "Nothing is being held back.")
				return nil
			}

			paths := make([]string, 0, len(retries))
			for p := range retries {
				paths = append(paths, p)
			}
			sort.Strings(paths)

			if list {
				fmt.Fprintf(out, "%d path(s) held back:\n\n", len(paths))
				for _, p := range paths {
					r := retries[p]
					fmt.Fprintf(out, "  %s\n     %d failure(s), next attempt %s%s\n",
						p, r.Failures, r.NextAttempt.Format(time.RFC3339), permanentNote(r))
					if r.LastError != "" {
						fmt.Fprintf(out, "     %s\n", r.LastError)
					}
				}
				fmt.Fprintln(out, "\nRun 'tendrils retry' to clear these and try again on the next pass.")
				return nil
			}

			cleared := 0
			for _, p := range paths {
				if err := store.ClearRetry(p); err != nil {
					fmt.Fprintf(out, "  could not clear %s: %v\n", p, err)
					continue
				}
				cleared++
			}
			fmt.Fprintf(out, "Cleared backoff on %d path(s); they will be attempted on the next pass.\n", cleared)
			return nil
		},
	}
	cmd.Flags().BoolVar(&list, "list", false, "show what is held back instead of clearing it")
	return cmd
}

func permanentNote(r index.Retry) string {
	if r.Permanent {
		return " (judged permanent)"
	}
	return ""
}
