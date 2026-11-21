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

// The retention commands (M3, plan-m3 §2): each is one /api/v1 call. The 7-day protection, the checks and
// the confirmations are all on the server.

type pendingSnap struct {
	ID          string     `json:"id"`
	ShortID     string     `json:"short_id"`
	Time        time.Time  `json:"time"`
	JobID       string     `json:"job_id"`
	Reason      string     `json:"reason"`
	KeptBy      int64      `json:"kept_by_revision"`
	RequestedBy string     `json:"requested_by"`
	PrunableAt  time.Time  `json:"prunable_at"`
	GoneBy      *time.Time `json:"really_gone_by"`
}

type pruneRun struct {
	ID         int64      `json:"id"`
	FQDN       string     `json:"fqdn"`
	Kind       string     `json:"kind"`
	Actor      string     `json:"actor"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Status     string     `json:"status"`
	Forgotten  []string   `json:"forgotten"`
	Pruned     bool       `json:"pruned"`
	Freed      string     `json:"freed"`
	Error      string     `json:"error"`
	GoneBy     *time.Time `json:"really_gone_by"`
}

func day(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Local().Format("2006-01-02")
}

func pendingCmd() *cobra.Command {
	return &cobra.Command{Use: "pending <fqdn>", Short: "What waits out the 7 days before it is forgotten: retention cuts and deletions", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		var p struct {
			Forget  []string      `json:"forget"`
			Pending []pendingSnap `json:"pending"`
			Jobs    []struct {
				JobID             string   `json:"job_id"`
				NotMadeByHoldfast []string `json:"not_made_by_holdfast"`
			} `json:"jobs"`
			NoncurrentDays int `json:"noncurrent_days"`
		}
		raw, err := call("GET", "/api/v1/hosts/"+url.PathEscape(a[0])+"/pending", nil, &p)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		if len(p.Pending) == 0 {
			fmt.Println("nothing is pending")
		} else {
			w := table()
			fmt.Fprintln(w, "SNAPSHOT\tTIME\tJOB\tWHY\tFORGOTTEN FROM\tREALLY GONE BY")
			for _, s := range p.Pending {
				why := fmt.Sprintf("retention cut; revision %d still keeps it", s.KeptBy)
				if s.Reason == "deletion" {
					why = "deletion asked by " + s.RequestedBy
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.ShortID, s.Time.Local().Format("2006-01-02 15:04"), s.JobID, why, s.PrunableAt.Local().Format("2006-01-02 15:04"), day(s.GoneBy))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Printf("\nholdfast job keep-longer <fqdn> <job> brings an older retention back; holdfast snapshot delete --cancel cancels a deletion\n")
		}
		if len(p.Forget) > 0 {
			fmt.Printf("%d snapshots are past the 7 days and go at the next daily run\n", len(p.Forget))
		}
		for _, j := range p.Jobs {
			if n := len(j.NotMadeByHoldfast); n > 0 {
				fmt.Printf("job %s: %d snapshots that Holdfast did not make are left alone\n", j.JobID, n)
			}
		}
		return nil
	}}
}

func prunesCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{Use: "prunes [fqdn]", Short: "Past forgets and prunes, newest first", Args: cobra.MaximumNArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		path := "/api/v1/prunes"
		if len(a) == 1 {
			path = "/api/v1/hosts/" + url.PathEscape(a[0]) + "/prunes"
		}
		if all {
			path += "?all=1"
		}
		var runs []pruneRun
		raw, err := call("GET", path, nil, &runs)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		w := table()
		fmt.Fprintln(w, "RUN\tSTARTED\tHOST\tKIND\tWHO\tSTATUS\tFORGOTTEN\tFREED\tREALLY GONE BY\tERROR")
		for _, r := range runs {
			freed := "—"
			if r.Pruned {
				freed = r.Freed
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", r.ID, r.StartedAt.Local().Format("2006-01-02 15:04"), r.FQDN, r.Kind, r.Actor, r.Status,
				len(r.Forgotten), freed, day(r.GoneBy), r.Error)
		}
		return w.Flush()
	}}
	c.Flags().BoolVar(&all, "all", false, "also the daily runs that found nothing to do")
	return c
}

// waitRun waits for a forget-and-prune to finish and returns it.
func waitRun(fqdn string, id int64, timeout time.Duration) (pruneRun, []byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		var runs []pruneRun
		raw, err := call("GET", "/api/v1/hosts/"+url.PathEscape(fqdn)+"/prunes?all=1&limit=1000", nil, &runs)
		if err != nil {
			return pruneRun{}, nil, err
		}
		for _, r := range runs {
			if r.ID == id && r.Status != "running" {
				b, _ := json.Marshal(r)
				return r, b, nil
			}
		}
		if time.Now().After(deadline) {
			return pruneRun{}, raw, fmt.Errorf("run %d did not finish within %s; holdfast prunes %s shows it", id, timeout, fqdn)
		}
		time.Sleep(2 * time.Second)
	}
}

func reportRun(r pruneRun, raw []byte) error {
	if asJSON {
		printJSON(raw)
	} else {
		fmt.Printf("run %d %s: %d snapshots forgotten", r.ID, r.Status, len(r.Forgotten))
		if r.Pruned {
			fmt.Printf(", pruned, %s freed", r.Freed)
			if r.GoneBy != nil {
				fmt.Printf("; S3's old versions expire by %s", day(r.GoneBy))
			}
		}
		fmt.Println()
	}
	if r.Status == "failed" {
		return errors.New(r.Error)
	}
	return nil
}

type runStarted struct {
	Run  int64  `json:"run"`
	FQDN string `json:"fqdn"`
}

func startedOrWait(raw []byte, res runStarted, wait bool, timeout time.Duration, what string) error {
	if !wait {
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("%s started as run %d; holdfast prunes %s shows how it ends (or use --wait)\n", what, res.Run, res.FQDN)
		return nil
	}
	r, b, err := waitRun(res.FQDN, res.Run, timeout)
	if err != nil {
		return err
	}
	return reportRun(r, b)
}

func retentionCmd() *cobra.Command {
	c := &cobra.Command{Use: "retention", Short: "Retention"}
	var wait bool
	var timeout time.Duration
	run := &cobra.Command{Use: "run <fqdn>", Short: "Run the daily forget and prune now, with the 7-day protection", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		var res runStarted
		raw, err := call("POST", "/api/v1/hosts/"+url.PathEscape(a[0])+"/retention/run", nil, &res)
		if err != nil {
			return err
		}
		return startedOrWait(raw, res, wait, timeout, "the daily run")
	}}
	run.Flags().BoolVar(&wait, "wait", false, "wait for the run to finish")
	run.Flags().DurationVar(&timeout, "timeout", time.Hour, "with --wait: how long to wait")
	c.AddCommand(run)
	return c
}

func pruneNowCmd() *cobra.Command {
	var confirm, msg string
	var wait bool
	var timeout time.Duration
	c := &cobra.Command{Use: "prune-now <fqdn>", Short: "Admin: forget what the current retention drops, without the 7-day protection, then prune. Without --confirm it only shows what would go",
		Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
			path := "/api/v1/hosts/" + url.PathEscape(a[0]) + "/prune-now"
			if confirm == "" {
				var p struct {
					Confirm   string        `json:"confirm"`
					Count     int           `json:"count"`
					Kept      int           `json:"kept"`
					From      *time.Time    `json:"from"`
					To        *time.Time    `json:"to"`
					Snapshots []pendingSnap `json:"snapshots"`
					Commands  []string      `json:"commands"`
					NotOurs   int           `json:"not_made_by_holdfast"`
					GoneBy    *time.Time    `json:"really_gone_by"`
				}
				raw, err := call("GET", path, nil, &p)
				if err != nil {
					return err
				}
				if asJSON {
					printJSON(raw)
					return nil
				}
				fmt.Printf("Prune now would forget %d snapshots", p.Count)
				if p.Count > 0 {
					fmt.Printf(", from %s to %s", p.From.Local().Format("2006-01-02 15:04"), p.To.Local().Format("2006-01-02 15:04"))
				}
				fmt.Printf(", without the 7-day protection, and then prune; it keeps %d.\n", p.Kept)
				w := table()
				fmt.Fprintln(w, "SNAPSHOT\tTIME\tJOB")
				for _, s := range p.Snapshots {
					fmt.Fprintf(w, "%s\t%s\t%s\n", s.ShortID, s.Time.Local().Format("2006-01-02 15:04"), s.JobID)
				}
				if err := w.Flush(); err != nil {
					return err
				}
				fmt.Println("\nthe restic commands, run by the server with its maintenance key:")
				for _, cmd := range p.Commands {
					fmt.Println("  " + cmd)
				}
				if p.NotOurs > 0 {
					fmt.Printf("%d snapshots that Holdfast did not make are left alone\n", p.NotOurs)
				}
				if p.GoneBy != nil {
					fmt.Printf("S3 keeps the deleted data's old versions until about %s (the bucket's lifecycle rule)\n", day(p.GoneBy))
				}
				fmt.Printf("\nto go ahead: holdfast prune-now %s --confirm %s\n", a[0], p.Confirm)
				return nil
			}
			var res runStarted
			raw, err := call("POST", path, map[string]any{"confirm": confirm, "message": msg}, &res)
			if err != nil {
				return err
			}
			return startedOrWait(raw, res, wait, timeout, "Prune now")
		}}
	c.Flags().StringVar(&confirm, "confirm", "", "the host's short name, typed to confirm")
	c.Flags().StringVarP(&msg, "message", "m", "", "why, for the audit log")
	c.Flags().BoolVar(&wait, "wait", false, "wait for the run to finish")
	c.Flags().DurationVar(&timeout, "timeout", time.Hour, "with --wait: how long to wait")
	return c
}

func snapshotDeleteCmd() *cobra.Command {
	var msg, confirm string
	var now, cancel, wait bool
	var timeout time.Duration
	c := &cobra.Command{Use: "delete <fqdn> <snapshot>", Short: "Admin: delete one snapshot after 7 days (--cancel undoes it), or now with --now --confirm <short name>",
		Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
			path := "/api/v1/hosts/" + url.PathEscape(a[0]) + "/snapshots/" + url.PathEscape(a[1]) + "/delete"
			if cancel {
				raw, err := call("DELETE", path, nil, nil)
				if err != nil {
					return err
				}
				if asJSON {
					printJSON(raw)
					return nil
				}
				fmt.Printf("the deletion of %s is cancelled\n", a[1])
				return nil
			}
			if now && confirm == "" {
				return fmt.Errorf("--now needs --confirm with the host's short name, %s", strings.SplitN(a[0], ".", 2)[0])
			}
			var res struct {
				SnapshotID string     `json:"snapshot_id"`
				Status     string     `json:"status"`
				PrunableAt *time.Time `json:"prunable_at"`
				Run        int64      `json:"run"`
				GoneBy     *time.Time `json:"really_gone_by"`
			}
			raw, err := call("POST", path, map[string]any{"message": msg, "now": now, "confirm": confirm}, &res)
			if err != nil {
				return err
			}
			if res.Run != 0 {
				return startedOrWait(raw, runStarted{Run: res.Run, FQDN: a[0]}, wait, timeout, "Delete now")
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			fmt.Printf("snapshot %s is pending deletion: the daily run forgets it from %s, and the next prune frees its space", res.SnapshotID[:min(8, len(res.SnapshotID))], res.PrunableAt.Local().Format("2006-01-02 15:04"))
			if res.GoneBy != nil {
				fmt.Printf("; S3's old versions expire by about %s", day(res.GoneBy))
			}
			fmt.Printf("\nholdfast snapshot delete %s %s --cancel undoes it\n", a[0], a[1])
			return nil
		}}
	c.Flags().StringVarP(&msg, "message", "m", "", "why, for the audit log")
	c.Flags().BoolVar(&now, "now", false, "Delete now: forget it at once, without the 7-day wait")
	c.Flags().StringVar(&confirm, "confirm", "", "with --now: the host's short name, typed to confirm")
	c.Flags().BoolVar(&cancel, "cancel", false, "cancel a pending deletion")
	c.Flags().BoolVar(&wait, "wait", false, "with --now: wait for the forget to finish")
	c.Flags().DurationVar(&timeout, "timeout", time.Hour, "with --wait: how long to wait")
	return c
}

func keepLongerCmd() *cobra.Command {
	var rev int64
	var msg string
	var accept []string
	c := &cobra.Command{Use: "keep-longer <fqdn> <job>", Short: "Bring back an older retention that was in force during the last 7 days (a new revision)", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
		in := map[string]any{"message": msg, "accept": accept}
		if rev != 0 {
			in["revision"] = rev
		}
		var res struct {
			Revision int64     `json:"revision"`
			Findings []finding `json:"findings"`
			Accepted []string  `json:"accepted_codes"`
		}
		raw, err := call("POST", "/api/v1/hosts/"+url.PathEscape(a[0])+"/jobs/"+url.PathEscape(a[1])+"/keep-longer", in, &res)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("saved as revision %d\n", res.Revision)
		printFindings(res.Findings, res.Accepted)
		return nil
	}}
	c.Flags().Int64Var(&rev, "revision", 0, "the revision whose retention to bring back (default: the newest that differs from today's)")
	c.Flags().StringVarP(&msg, "message", "m", "", "revision message")
	c.Flags().StringSliceVar(&accept, "accept", nil, "accept these warning codes")
	return c
}
