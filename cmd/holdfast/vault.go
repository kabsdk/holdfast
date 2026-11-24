package main

import (
	"fmt"
	"net/url"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// Off-site copies (decisions 027, 039 and 041), when server.yaml has offsite:. `holdfast offsite copies` lists a
// host's copies, for restore --from-offsite. Exclude, Include and Match primary act on the off-site jobs, as every
// command does with --offsite.

func offsiteCmd() *cobra.Command {
	c := &cobra.Command{Use: "offsite", Short: "Off-site copies: a host's copies, and the off-site jobs' Exclude, Include and Match primary"}
	c.AddCommand(&cobra.Command{Use: "copies <agent>", Short: "The off-site copies of a host, as last reported (for restore --from-offsite)",
		Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
			var copies []struct {
				ID       string    `json:"id"`
				Original string    `json:"original"`
				Time     time.Time `json:"time"`
				JobID    string    `json:"job_id"`
			}
			raw, err := call("GET", "/api/v1/agents/"+url.PathEscape(a[0])+"/offsite", nil, &copies)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "COPY\tORIGINAL\tTIME\tJOB")
			for _, cp := range copies {
				fmt.Fprintf(w, "%.8s\t%.8s\t%s\t%s\n", cp.ID, cp.Original, cp.Time.Local().Format("2006-01-02 15:04"), cp.JobID)
			}
			return w.Flush()
		}})
	c.AddCommand(excludeCmd(true), excludeCmd(false))
	var msg string
	var accept []string
	match := &cobra.Command{Use: "match-primary <agent> <job>", Short: "Make a job's off-site retention its primary retention again, following every change there",
		Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
			useOffsite = true
			var res struct {
				Revision int64     `json:"revision"`
				Findings []finding `json:"findings"`
				Accepted []string  `json:"accepted_codes"`
			}
			raw, err := call("POST", "/api/v1/agents/"+url.PathEscape(a[0])+"/jobs/"+url.PathEscape(a[1])+"/match-primary",
				map[string]any{"message": msg, "accept": accept}, &res)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			fmt.Printf("saved as off-site revision %d\n", res.Revision)
			printFindings(res.Findings, res.Accepted)
			return nil
		}}
	match.Flags().StringVarP(&msg, "message", "m", "", "revision message")
	match.Flags().StringSliceVar(&accept, "accept", nil, "accept these warning codes (R104 when primary keeps less)")
	c.AddCommand(match)
	return c
}

// excludeCmd is Exclude, or Include, of an off-site job.
func excludeCmd(exclude bool) *cobra.Command {
	use, short := "include <agent> <job>", "Copy an excluded job off-site again"
	if exclude {
		use, short = "exclude <agent> <job>", "Stop copying a job off-site; its copies stay under its off-site retention"
	}
	var msg string
	c := &cobra.Command{Use: use, Short: short, Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
		useOffsite = true
		path := "/api/v1/agents/" + url.PathEscape(a[0]) + "/jobs/" + url.PathEscape(a[1]) + "/include"
		if exclude {
			path = "/api/v1/agents/" + url.PathEscape(a[0]) + "/jobs/" + url.PathEscape(a[1]) + "/exclude"
		}
		var res struct {
			Revision int64 `json:"revision"`
		}
		raw, err := call("POST", path, map[string]any{"message": msg}, &res)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("off-site revision %d\n", res.Revision)
		return nil
	}}
	c.Flags().StringVarP(&msg, "message", "m", "", "revision message")
	return c
}
