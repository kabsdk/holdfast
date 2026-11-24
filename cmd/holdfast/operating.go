package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Operating Holdfast: rotating a compromised host's password, and decommission. The checks and
// the wait are on the server; the CLI only asks.

func hostOperatingCmds() []*cobra.Command {
	var rotateMsg string
	rotate := &cobra.Command{Use: "rotate <agent>", Short: "After a compromise: a new repository password, the agent's certificate unpinned, and a new enrollment code (docs/runbooks/compromised-host.md)",
		Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
			var res struct {
				CredentialsVersion int        `json:"credentials_version"`
				NewKey             string     `json:"new_key"`
				RemovedKeys        []string   `json:"removed_keys"`
				Escrowed           bool       `json:"escrowed"`
				Warning            string     `json:"warning"`
				Enrollment         enrollment `json:"enrollment"`
			}
			raw, err := call("POST", "/api/v1/agents/"+url.PathEscape(a[0])+"/rotate", map[string]any{"message": rotateMsg}, &res)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			fmt.Printf("%s has a new repository password (key %s, credentials version %d); removed key %s.\n"+
				"Its agent's certificate is unpinned: the host gets nothing more until it enrolls again.\n",
				a[0], res.NewKey, res.CredentialsVersion, strings.Join(res.RemovedKeys, ", "))
			if res.Escrowed {
				fmt.Println("The escrow file in the bucket holds the new password.")
			}
			fmt.Printf("\nOnce the host is clean or rebuilt, enroll it again.\n%s", res.Enrollment.howTo())
			fmt.Println("\nEnrolling makes the host a new key of its own; the old certificate no longer works.")
			if res.Warning != "" {
				fmt.Println("\nwarning:", res.Warning)
			}
			return nil
		}}
	rotate.Flags().StringVarP(&rotateMsg, "message", "m", "", "why, for the audit log")

	var confirm, msg string
	var cancel bool
	decommission := &cobra.Command{Use: "decommission <agent>", Short: "Admin: delete a removed host's repository after 7 days (--cancel stops it); needs --confirm <agent>",
		Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
			path := "/api/v1/agents/" + url.PathEscape(a[0]) + "/decommission"
			if cancel {
				raw, err := call("DELETE", path, map[string]any{"message": msg}, nil)
				if err != nil {
					return err
				}
				if asJSON {
					printJSON(raw)
					return nil
				}
				fmt.Printf("the decommission of %s is cancelled; its repository stays\n", a[0])
				return nil
			}
			var d struct {
				DueAt time.Time `json:"due_at"`
			}
			raw, err := call("POST", path, map[string]any{"confirm": confirm, "message": msg}, &d)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			fmt.Printf("the repository of %s and its escrow file are deleted on %s.\n"+
				"Until then, holdfast agent decommission %s --cancel stops it. Afterwards, S3 keeps the old versions until\n"+
				"the bucket's lifecycle rule expires them; holdfast agent show %s then says when the data is really gone.\n",
				a[0], d.DueAt.Local().Format("2006-01-02 15:04"), a[0], a[0])
			return nil
		}}
	decommission.Flags().StringVar(&confirm, "confirm", "", "the host's FQDN, typed to confirm")
	decommission.Flags().StringVarP(&msg, "message", "m", "", "why, for the audit log")
	decommission.Flags().BoolVar(&cancel, "cancel", false, "cancel a decommission that is still waiting")
	return []*cobra.Command{rotate, decommission, rollbackCmd(), rollbacksCmd()}
}

// rollbackCmd shows what rolling a repository back to a time would change, and with --confirm does it
// (decision 038).
func rollbackCmd() *cobra.Command {
	var to, confirm, msg string
	var agentDown, wait bool
	c := &cobra.Command{Use: "rollback <agent> --to <time>", Short: "Admin: roll the host's repository back to a time, from S3's old versions; without --confirm it only shows what would change",
		Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
			t, err := parseTime(to)
			if err != nil {
				return err
			}
			if confirm == "" {
				var p struct {
					To                time.Time      `json:"to"`
					Refused           string         `json:"refused"`
					Oldest            time.Time      `json:"oldest"`
					BringBack         map[string]int `json:"bring_back"`
					SetAside          map[string]int `json:"set_aside"`
					Unchanged         int            `json:"unchanged"`
					SnapshotsBack     []string       `json:"snapshots_back"`
					SnapshotsSetAside []string       `json:"snapshots_set_aside"`
					ForgottenBefore   []string       `json:"forgotten_before"`
					Deletions         []time.Time    `json:"snapshot_deletions"`
				}
				raw, err := call("GET", "/api/v1/agents/"+url.PathEscape(a[0])+"/rollback?to="+url.QueryEscape(t.Format(time.RFC3339)), nil, &p)
				if err != nil {
					return err
				}
				if asJSON {
					printJSON(raw)
					return nil
				}
				fmt.Printf("rolling %s back to %s (the earliest possible: %s)\n", a[0], p.To.Local().Format("2006-01-02 15:04:05"), p.Oldest.Local().Format("2006-01-02 15:04"))
				if p.Refused != "" {
					fmt.Println("refused:", p.Refused)
					return nil
				}
				kinds := func(m map[string]int) string {
					var parts []string
					for _, k := range []string{"config", "keys", "snapshots", "index", "data", "other"} {
						if m[k] > 0 {
							parts = append(parts, fmt.Sprintf("%d %s", m[k], k))
						}
					}
					if len(parts) == 0 {
						return "none"
					}
					return strings.Join(parts, ", ")
				}
				fmt.Printf("brought back: %s\nset aside:    %s\nunchanged:    %d objects\n", kinds(p.BringBack), kinds(p.SetAside), p.Unchanged)
				fmt.Printf("snapshots that come back (%d): %s\n", len(p.SnapshotsBack), strings.Join(short(p.SnapshotsBack), " "))
				fmt.Printf("snapshots set aside (%d): %s\n", len(p.SnapshotsSetAside), strings.Join(short(p.SnapshotsSetAside), " "))
				if len(p.ForgottenBefore) > 0 {
					fmt.Printf("of those that come back, Holdfast had forgotten %d; the daily run forgets them again unless a retention keeps them\n", len(p.ForgottenBefore))
				}
				for _, d := range p.Deletions {
					fmt.Printf("  a snapshot file was deleted at %s\n", d.Local().Format("2006-01-02 15:04:05"))
				}
				fmt.Printf("\nNothing is deleted: what is set aside stays in S3 as old versions, and this can be rolled back too.\n"+
					"to go ahead: holdfast agent rollback %s --to %s --confirm %s\n", a[0], t.UTC().Format(time.RFC3339), a[0])
				return nil
			}
			var r rollbackRow
			raw, err := call("POST", "/api/v1/agents/"+url.PathEscape(a[0])+"/rollback", map[string]any{"to": t, "confirm": confirm, "agent_down": agentDown, "message": msg}, &r)
			if err != nil {
				return err
			}
			if wait {
				for r.Status == "running" {
					time.Sleep(2 * time.Second)
					var list []rollbackRow
					if raw, err = call("GET", "/api/v1/agents/"+url.PathEscape(a[0])+"/rollbacks", nil, &list); err != nil {
						return err
					}
					for _, x := range list {
						if x.ID == r.ID {
							r = x
						}
					}
				}
			}
			if asJSON {
				if wait { // the rollback itself, as it ended
					raw, _ = json.Marshal(r)
				}
				printJSON(raw)
			} else {
				printRollback(r)
			}
			if r.Status == "failed" {
				return errors.New("the rollback failed")
			}
			return nil
		}}
	c.Flags().StringVar(&to, "to", "", "the time to roll back to, such as 2026-11-20T03:00:00Z or \"2026-11-20 03:00\" (local)")
	c.Flags().StringVar(&confirm, "confirm", "", "the host's FQDN, typed to go ahead")
	c.Flags().BoolVar(&agentDown, "agent-down", false, "the host is down, so its agent cannot be paused (only for an agent that is offline)")
	c.Flags().BoolVar(&wait, "wait", false, "with --confirm: wait until it is done")
	c.Flags().StringVarP(&msg, "message", "m", "", "why, for the audit log")
	_ = c.MarkFlagRequired("to")
	return c
}

type rollbackRow struct {
	ID                int64      `json:"id"`
	To                time.Time  `json:"to"`
	Actor             string     `json:"actor"`
	RequestedAt       time.Time  `json:"requested_at"`
	FinishedAt        *time.Time `json:"finished_at"`
	Status            string     `json:"status"`
	BroughtBack       int        `json:"brought_back"`
	SetAside          int        `json:"set_aside"`
	SnapshotsBack     []string   `json:"snapshots_back"`
	SnapshotsSetAside []string   `json:"snapshots_set_aside"`
	CheckPassed       *bool      `json:"check_passed"`
	Error             string     `json:"error"`
}

func printRollback(r rollbackRow) {
	check := "not run"
	if r.CheckPassed != nil {
		check = map[bool]string{true: "passed", false: "FAILED"}[*r.CheckPassed]
	}
	fmt.Printf("rollback %d to %s, by %s: %s; %d objects brought back, %d set aside; %d snapshots back, %d set aside; restic check %s\n",
		r.ID, r.To.Local().Format("2006-01-02 15:04:05"), r.Actor, r.Status, r.BroughtBack, r.SetAside, len(r.SnapshotsBack), len(r.SnapshotsSetAside), check)
	if r.Error != "" {
		fmt.Println("  ", r.Error)
	}
}

func rollbacksCmd() *cobra.Command {
	return &cobra.Command{Use: "rollbacks <agent>", Short: "A host's repository rollbacks", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		var list []rollbackRow
		raw, err := call("GET", "/api/v1/agents/"+url.PathEscape(a[0])+"/rollbacks", nil, &list)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		if len(list) == 0 {
			fmt.Println("no rollbacks")
		}
		for _, r := range list {
			printRollback(r)
		}
		return nil
	}}
}

// parseTime reads RFC 3339, or "2006-01-02 15:04[:05]" in local time.
func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--to %q: give a time such as 2026-11-20T03:00:00Z or \"2026-11-20 03:00\"", s)
}

func short(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id
		if len(id) > 8 {
			out[i] = id[:8]
		}
	}
	return out
}

// sizeOf prints bytes in binary units, as restic does.
func sizeOf(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// printOperating prints a host's size, escrow and decommission, for host show.
func printOperating(h hostView) {
	if d := h.Decommission; d != nil {
		switch {
		case d.DoneAt != nil:
			gone := "when the bucket's lifecycle rule expires S3's old versions"
			if d.GoneAt != nil {
				gone = "on " + d.GoneAt.Local().Format("2006-01-02")
			}
			fmt.Printf("decommissioned on %s (%d objects deleted, as %s asked); the data is really gone %s\n",
				d.DoneAt.Local().Format("2006-01-02 15:04"), d.Objects, d.RequestedBy, gone)
			return
		default:
			fmt.Printf("DECOMMISSION: the repository is deleted on %s, as %s asked; holdfast agent decommission %s --cancel stops it\n",
				d.DueAt.Local().Format("2006-01-02 15:04"), d.RequestedBy, h.FQDN)
			if d.Error != "" {
				fmt.Println("  the last attempt failed:", d.Error)
			}
		}
	}
	if h.RepoKeyRetiring != "" {
		fmt.Printf("ALERT: the old repository password still opens the repository (key %s): run holdfast agent rotate %s again\n", h.RepoKeyRetiring, h.FQDN)
	}
	if s := h.Size; s != nil {
		fmt.Printf("repository: %s in %d objects, and %s in old versions kept by S3 (measured %s)\n",
			sizeOf(s.Current), s.Objects, sizeOf(s.Noncurrent), s.At.Local().Format("2006-01-02 15:04"))
	}
	if h.SizeError != "" {
		fmt.Println("warning: the repository's size could not be measured:", h.SizeError)
	}
	switch {
	case h.Escrow.Current:
		fmt.Printf("escrow: the bucket's escrow file holds the current password (written %s)\n", ago(h.Escrow.WrittenAt))
	case h.Escrow.Error != "":
		fmt.Println("warning: the escrow file could not be written:", h.Escrow.Error)
	}
}
