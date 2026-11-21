package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The snapshot commands (M2): each is one /api/v1 call, rendered as a table.

type snapshotRow struct {
	ID             string     `json:"id"`
	ShortID        string     `json:"short_id"`
	Time           time.Time  `json:"time"`
	JobID          string     `json:"job_id"`
	Paths          []string   `json:"paths"`
	TotalFiles     *int64     `json:"total_files"`
	TotalBytes     *int64     `json:"total_bytes"`
	MadeByHoldfast bool       `json:"made_by_holdfast"`
	GoneAt         *time.Time `json:"gone_at"`
	DeletedByUs    bool       `json:"deleted_by_holdfast"`
	DeletionAsked  *time.Time `json:"deletion_requested_at"`
	ReallyGoneBy   *time.Time `json:"really_gone_by"`
}

type node struct {
	Name, Type, Path, Permissions string
	UID, GID                      uint32
	Size                          uint64
	MTime                         time.Time `json:"mtime"`
}

func human(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func searchCmd() *cobra.Command {
	return &cobra.Command{Use: "search <text>", Short: "Find hosts, jobs and snapshot IDs, including snapshots that are gone", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		var res struct {
			Hosts []string
			Jobs  []struct {
				FQDN  string
				JobID string `json:"job_id"`
			}
			Snapshots []struct {
				FQDN    string
				ShortID string    `json:"short_id"`
				Time    time.Time `json:"time"`
				JobID   string    `json:"job_id"`
				State   string
			}
		}
		raw, err := call("GET", "/api/v1/search?"+url.Values{"q": {a[0]}}.Encode(), nil, &res)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		for _, h := range res.Hosts {
			fmt.Printf("host      %s\n", h)
		}
		for _, j := range res.Jobs {
			fmt.Printf("job       %s/%s\n", j.FQDN, j.JobID)
		}
		for _, s := range res.Snapshots {
			state := ""
			if s.State != "present" {
				state = "  " + strings.ToUpper(s.State)
			}
			fmt.Printf("snapshot  %s  %s  %s  %s%s\n", s.ShortID, s.FQDN, s.JobID, s.Time.Local().Format("2006-01-02 15:04"), state)
		}
		return nil
	}}
}

func snapshotCmd() *cobra.Command {
	c := &cobra.Command{Use: "snapshot", Short: "Snapshots: list, files, changes and find (names and metadata only)"}

	var job, month string
	var all, refresh bool
	list := &cobra.Command{Use: "list <fqdn>", Short: "A host's snapshots, newest first", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		q := url.Values{}
		if job != "" {
			q.Set("job", job)
		}
		if month != "" {
			q.Set("month", month)
		}
		if all {
			q.Set("all", "1")
		}
		if refresh {
			q.Set("refresh", "1")
		}
		var rows []snapshotRow
		raw, err := call("GET", "/api/v1/hosts/"+url.PathEscape(a[0])+"/snapshots?"+q.Encode(), nil, &rows)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		w := table()
		fmt.Fprintln(w, "SNAPSHOT\tTIME\tJOB\tFILES\tSIZE\tPATHS\tNOTE")
		for _, s := range rows {
			files, size := "—", "—"
			if s.TotalFiles != nil {
				files = strconv.FormatInt(*s.TotalFiles, 10)
			}
			if s.TotalBytes != nil {
				size = human(uint64(*s.TotalBytes))
			}
			note := ""
			switch {
			case s.GoneAt != nil && s.DeletedByUs:
				note = "deleted by Holdfast " + s.GoneAt.Local().Format("2006-01-02 15:04")
				if s.ReallyGoneBy != nil {
					note += ", S3's old versions expire by " + s.ReallyGoneBy.Local().Format("2006-01-02")
				} else {
					note += ", data freed at the next prune"
				}
			case s.GoneAt != nil:
				note = "GONE " + s.GoneAt.Local().Format("2006-01-02 15:04")
			case s.DeletionAsked != nil:
				note = "deletion pending since " + s.DeletionAsked.Local().Format("2006-01-02 15:04")
			case !s.MadeByHoldfast:
				note = "not made by Holdfast"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.ShortID, s.Time.Local().Format("2006-01-02 15:04"), s.JobID, files, size, strings.Join(s.Paths, " "), note)
		}
		return w.Flush()
	}}
	list.Flags().StringVar(&job, "job", "", "only this job")
	list.Flags().StringVar(&month, "month", "", "only this month, such as 2026-11")
	list.Flags().BoolVar(&all, "all", false, "also snapshots that are no longer in the repository")
	list.Flags().BoolVar(&refresh, "refresh", false, "read the repository first")
	c.AddCommand(list, snapshotDeleteCmd())

	var filesPage int
	files := &cobra.Command{Use: "files <fqdn> <snapshot> [path]", Short: "One directory of a snapshot: names and metadata", Args: cobra.RangeArgs(2, 3), RunE: func(_ *cobra.Command, a []string) error {
		q := url.Values{"page": {strconv.Itoa(filesPage)}}
		if len(a) == 3 {
			q.Set("path", a[2])
		}
		var p struct {
			Path               string
			Entries            []node
			Total, Page, Pages int
		}
		raw, err := call("GET", "/api/v1/hosts/"+url.PathEscape(a[0])+"/snapshots/"+url.PathEscape(a[1])+"/files?"+q.Encode(), nil, &p)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		w := table()
		fmt.Fprintln(w, "MODE\tUID\tGID\tSIZE\tMODIFIED\tNAME")
		for _, n := range p.Entries {
			name := n.Name
			if n.Type == "dir" {
				name += "/"
			}
			fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\t%s\n", n.Permissions, n.UID, n.GID, human(n.Size), n.MTime.Local().Format("2006-01-02 15:04"), name)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Printf("\n%s: %d entries, page %d of %d\n", p.Path, p.Total, p.Page, p.Pages)
		return nil
	}}
	files.Flags().IntVar(&filesPage, "page", 1, "page of 1000 entries")
	c.AddCommand(files)

	var against string
	var changesPage int
	changes := &cobra.Command{Use: "changes <fqdn> <snapshot>", Short: "What changed since the previous snapshot of its job, or since --against", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
		q := url.Values{"page": {strconv.Itoa(changesPage)}}
		if against != "" {
			q.Set("against", against)
		}
		var v struct {
			Snapshot, Against                  string
			Added, Removed, Total, Page, Pages int
			Content                            int `json:"content_changed"`
			Metadata                           int `json:"metadata_changed"`
			TypeChanged                        int `json:"type_changed"`
			Changes                            []struct{ Path, Modifier string }
		}
		raw, err := call("GET", "/api/v1/hosts/"+url.PathEscape(a[0])+"/snapshots/"+url.PathEscape(a[1])+"/changes?"+q.Encode(), nil, &v)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("%.8s against %.8s: %d added, %d removed, %d content changed, %d metadata changed, %d type changed\n\n",
			v.Snapshot, v.Against, v.Added, v.Removed, v.Content, v.Metadata, v.TypeChanged)
		for _, ch := range v.Changes {
			fmt.Printf("%s  %s\n", ch.Modifier, ch.Path)
		}
		if v.Pages > 1 {
			fmt.Printf("\npage %d of %d\n", v.Page, v.Pages)
		}
		return nil
	}}
	changes.Flags().StringVar(&against, "against", "", "compare with this older snapshot")
	changes.Flags().IntVar(&changesPage, "page", 1, "page of 1000 changes")
	c.AddCommand(changes)

	c.AddCommand(&cobra.Command{Use: "find <fqdn> <pattern>", Short: "Find a file in all of a host's snapshots", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
		var found []struct {
			ShortID string    `json:"short_id"`
			Time    time.Time `json:"time"`
			JobID   string    `json:"job_id"`
			Matches []node
		}
		raw, err := call("GET", "/api/v1/hosts/"+url.PathEscape(a[0])+"/find?"+url.Values{"pattern": {a[1]}}.Encode(), nil, &found)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		w := table()
		fmt.Fprintln(w, "SNAPSHOT\tTIME\tJOB\tSIZE\tMODIFIED\tPATH")
		for _, f := range found {
			for _, m := range f.Matches {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", f.ShortID, f.Time.Local().Format("2006-01-02 15:04"), f.JobID, human(m.Size), m.MTime.Local().Format("2006-01-02 15:04"), m.Path)
			}
		}
		return w.Flush()
	}})
	return c
}
