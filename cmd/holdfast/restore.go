package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/spf13/cobra"
)

type restoreEvent struct {
	FQDN       string            `json:"fqdn"`
	Kind       string            `json:"kind"`
	CommandID  string            `json:"command_id"`
	Status     string            `json:"status"`
	Finished   time.Time         `json:"finished"`
	TotalFiles int64             `json:"total_files"`
	TotalBytes int64             `json:"total_bytes"`
	Error      string            `json:"error"`
	Detail     map[string]string `json:"detail"`
}

func restoreCmd() *cobra.Command {
	var to string
	var inPlace, dryRun, wait bool
	var leaveOut, only []string
	c := &cobra.Command{Use: "restore <fqdn> <snapshot>", Short: "Restore a snapshot on its host, through the agent (a new root-only directory by default)",
		Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
			in := map[string]any{"target": "new", "exclude": leaveOut, "include": only, "dry_run": dryRun}
			switch {
			case inPlace && to != "":
				return errors.New("give --in-place or --to, not both")
			case inPlace:
				in["target"] = "in_place"
			case to != "":
				in["target"], in["directory"] = "directory", to
			}
			var res struct {
				Command   string    `json:"command"`
				ExpiresAt time.Time `json:"expires_at"`
			}
			raw, err := call("POST", "/api/v1/hosts/"+url.PathEscape(a[0])+"/snapshots/"+url.PathEscape(a[1])+"/restore", in, &res)
			if err != nil {
				return err
			}
			if !wait {
				if asJSON {
					printJSON(raw)
					return nil
				}
				fmt.Printf("restore %s queued for %s; the host must pick it up by %s\nholdfast restores shows its result\n", res.Command, a[0], res.ExpiresAt.Local().Format("2006-01-02 15:04"))
				return nil
			}
			for {
				var evs []json.RawMessage
				if _, err := call("GET", "/api/v1/hosts/"+url.PathEscape(a[0])+"/events?limit=200", nil, &evs); err != nil {
					return err
				}
				for _, rawEv := range evs {
					var ev restoreEvent
					if json.Unmarshal(rawEv, &ev) != nil || ev.Kind != "restore" || ev.CommandID != res.Command {
						continue
					}
					if asJSON {
						printJSON(rawEv)
					} else {
						printRestore(ev)
					}
					if ev.Status != "success" {
						return errors.New("the restore did not succeed")
					}
					return nil
				}
				if time.Now().After(res.ExpiresAt) {
					return errors.New("the restore was not picked up in time")
				}
				time.Sleep(2 * time.Second)
			}
		}}
	c.Flags().StringVar(&to, "to", "", "restore into this directory (the host must allow it)")
	c.Flags().BoolVar(&inPlace, "in-place", false, "restore over the original files, after a safety backup (the host must allow it)")
	c.Flags().StringSliceVar(&leaveOut, "leave-out", nil, "a path to leave out (repeat)")
	c.Flags().StringSliceVar(&only, "only", nil, "restore only this path (repeat)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "only report what would be restored")
	c.Flags().BoolVar(&wait, "wait", false, "wait for the agent's result")
	return c
}

func printRestore(ev restoreEvent) {
	fmt.Printf("%s  %s  %s  %s", ev.Finished.Local().Format("2006-01-02 15:04"), ev.FQDN, ev.Status, ev.Detail["target"])
	if d := ev.Detail["directory"]; d != "" {
		fmt.Printf(" into %s", d)
	}
	if ev.Detail["dry_run"] == "true" {
		fmt.Print(" (dry run)")
	}
	fmt.Printf(": %s of %d files, %s", ev.Detail["files_restored"], ev.TotalFiles, human(uint64(ev.TotalBytes)))
	if p := ev.Detail["pre_restore"]; p != "" {
		fmt.Printf("; safety backup %.8s", p)
	}
	if ev.Error != "" {
		fmt.Printf("\n    %s", ev.Error)
	}
	fmt.Println()
}

func restoresCmd() *cobra.Command {
	return &cobra.Command{Use: "restores", Short: "Every host's restores: queued, then finished", RunE: func(*cobra.Command, []string) error {
		var v struct {
			Queued []struct {
				ID, FQDN  string
				CreatedBy string    `json:"created_by"`
				CreatedAt time.Time `json:"created_at"`
			}
			Done []restoreEvent
		}
		raw, err := call("GET", "/api/v1/restores", nil, &v)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		for _, q := range v.Queued {
			fmt.Printf("queued  %s  %s  by %s at %s\n", q.ID, q.FQDN, q.CreatedBy, q.CreatedAt.Local().Format("2006-01-02 15:04"))
		}
		for _, ev := range v.Done {
			printRestore(ev)
		}
		return nil
	}}
}
