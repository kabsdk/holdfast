// holdfast is the CLI: a thin client of the /api/v1 API (decision 028). It turns flags into API
// requests and API answers into tables; every check and rule is on the server.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

var (
	socketPath string
	asJSON     bool
	// useOffsite sends every command to the off-site copies' API, /api/v1/offsite/ (decision 041): the same
	// commands, on the off-site repositories, their retention, their history and their audit log.
	useOffsite bool
)

func main() {
	root := &cobra.Command{Use: "holdfast", Short: "Holdfast command line (a client of the Holdfast API)", SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Name() == "login" || cmd.Name() == "logout" {
				return nil
			}
			var err error
			current, err = resolveTarget(cmd)
			return err
		}}
	root.PersistentFlags().StringVar(&socketPath, "socket", envOr("HOLDFAST_SOCKET", "/run/holdfast/api.sock"), "API socket of holdfast-server (root on the server)")
	root.PersistentFlags().StringVar(&serverURL, "server", os.Getenv("HOLDFAST_SERVER"), "the server's URL, for holdfast login; later commands use the saved login")
	root.PersistentFlags().BoolVar(&asJSON, "json", false, "print the API's JSON answer")
	root.PersistentFlags().BoolVar(&useOffsite, "offsite", false, "act on the off-site copies: their hosts, retention, history and audit log")
	root.AddCommand(statusCmd(), hostCmd(), jobCmd(), snapshotCmd(), restoreCmd(), restoresCmd(), searchCmd(), revisionCmd(), auditCmd(), checkCmd(), escrowCmd(),
		pendingCmd(), prunesCmd(), retentionCmd(), pruneNowCmd(), loginCmd(), logoutCmd(), whoamiCmd(), accessCmd(), settingsCmd(), offsiteCmd(), exportCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

/* ---------------- API client ---------------- */

type apiError struct {
	Error    string `json:"error"`
	Problems []struct {
		Path    string `json:"path"`
		Message string `json:"message"`
	} `json:"problems"`
	Storage *storageReport `json:"storage"`
}

// storageReport is the server's storage contract test (decision 029).
type storageReport struct {
	CheckedAt time.Time `json:"checked_at"`
	Findings  []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"findings"`
	ObjectLock string `json:"object_lock"` // reported, not required (decision 041)
}

func (r *storageReport) lines() string {
	var b strings.Builder
	for _, f := range r.Findings {
		b.WriteString("\n  " + f.Code + "  " + f.Message)
	}
	return b.String()
}

// call sends a request and decodes the answer into out. It returns the raw body for --json. With --offsite it goes to
// the off-site copies' API: the same paths, under /api/v1/offsite/.
func call(method, path string, in, out any) ([]byte, error) {
	if useOffsite {
		path = strings.Replace(path, "/api/v1/", "/api/v1/offsite/", 1)
	}
	resp, err := current.do(method, path, in)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e apiError
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg := e.Error
			for _, p := range e.Problems {
				msg += "\n  " + p.Path + ": " + p.Message
			}
			if e.Storage != nil && len(e.Storage.Findings) > 0 {
				msg = "the storage does not meet the contract (docs/storage.md):" + e.Storage.lines()
			}
			return raw, errors.New(msg)
		}
		return raw, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return raw, err
		}
	}
	return raw, nil
}

func printJSON(raw []byte) {
	var v any
	_ = json.Unmarshal(raw, &v)
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func table() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0) }

func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04")
}

/* ---------------- commands ---------------- */

func statusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Show the server's state", RunE: func(*cobra.Command, []string) error {
		var st map[string]any
		raw, err := call("GET", "/api/v1/status", nil, &st)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("head revision %v · node %v · default zone %v · you are %v\n", st["head_revision"], st["node"], st["default_zone"], st["actor"])
		// The server's own problems, such as its nightly export (decision 040); a host's are on its page.
		ps, _ := st["problems"].([]any)
		for _, p := range ps {
			if m, ok := p.(map[string]any); ok {
				fmt.Printf("%v: %v\n", m["severity"], m["message"])
			}
		}
		return nil
	}}
}

type jobStatus struct {
	LastRunAt      *time.Time `json:"last_run_at"`
	LastStatus     string     `json:"last_status"`
	LastError      string     `json:"last_error"`
	LastSuccessAt  *time.Time `json:"last_success_at"`
	LastSnapshotID string     `json:"last_snapshot_id"`
}

type hostView struct {
	FQDN              string         `json:"fqdn"`
	State             string         `json:"state"`
	Progress          string         `json:"progress"`
	Online            bool           `json:"online"`
	InSync            bool           `json:"in_sync"`
	DesiredRevision   int64          `json:"desired_revision"`
	FirstBackupAt     *time.Time     `json:"first_backup_at"`
	Storage           *storageReport `json:"storage"`
	ResticWarning     string         `json:"restic_warning"`
	AgentTooOld       bool           `json:"agent_too_old"`
	Snapshots         int            `json:"snapshots"`
	NotMadeByHoldfast int            `json:"not_made_by_holdfast"`
	Disappeared       int            `json:"disappeared"`
	ForeignKeys       []string       `json:"foreign_keys"`
	CatalogueError    string         `json:"catalogue_error"`
	Escrow            struct {
		Current   bool       `json:"current"`
		WrittenAt *time.Time `json:"written_at"`
		Error     string     `json:"error"`
	} `json:"escrow"`
	Size *struct {
		Current    int64     `json:"current_bytes"`
		Noncurrent int64     `json:"noncurrent_bytes"`
		Objects    int       `json:"objects"`
		At         time.Time `json:"measured_at"`
	} `json:"size"`
	SizeError       string `json:"size_error"`
	RepoKeyRetiring string `json:"repo_key_retiring"`
	Decommission    *struct {
		RequestedBy string     `json:"requested_by"`
		DueAt       time.Time  `json:"due_at"`
		DoneAt      *time.Time `json:"done_at"`
		Objects     int        `json:"objects_deleted"`
		GoneAt      *time.Time `json:"really_gone_at"`
		Error       string     `json:"error"`
	} `json:"decommission"`
	Agent *struct {
		LastSeenAt      *time.Time `json:"last_seen_at"`
		AgentVersion    string     `json:"agent_version"`
		ResticVersion   string     `json:"restic_version"`
		OSID            string     `json:"os_id"`
		OSVersion       string     `json:"os_version"`
		AppliedRevision int64      `json:"applied_revision"`
	} `json:"agent"`
	Jobs []struct {
		ID            string      `json:"id"`
		Paths         []string    `json:"paths"`
		NextRuns      []time.Time `json:"next_runs"`
		LongestGap    string      `json:"longest_gap"`
		WorstCaseLoss string      `json:"worst_case_loss"`
		Status        *jobStatus  `json:"status"`
	} `json:"jobs"`
}

func hostStatus(h hostView) string {
	switch {
	case h.State == "awaiting_enrollment":
		return "waiting for agent"
	case !h.Online:
		return "agent offline"
	case h.Storage != nil && len(h.Storage.Findings) > 0:
		return "storage check failed"
	case !h.InSync:
		return "config drift"
	}
	return "ok"
}

func hostCmd() *cobra.Command {
	c := &cobra.Command{Use: "host", Short: "Hosts"}
	c.AddCommand(&cobra.Command{Use: "list", Short: "List hosts", RunE: func(*cobra.Command, []string) error {
		var hosts []hostView
		raw, err := call("GET", "/api/v1/hosts", nil, &hosts)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		w := table()
		fmt.Fprintln(w, "HOST\tSTATUS\tPROGRESS\tJOBS\tLAST SEEN")
		for _, h := range hosts {
			seen := "—"
			if h.Agent != nil {
				seen = ago(h.Agent.LastSeenAt)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", h.FQDN, hostStatus(h), h.Progress, len(h.Jobs), seen)
		}
		return w.Flush()
	}})
	c.AddCommand(&cobra.Command{Use: "show <fqdn>", Short: "Show a host: agent, drift and jobs", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		var h hostView
		raw, err := call("GET", "/api/v1/hosts/"+url.PathEscape(a[0]), nil, &h)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("%s  ·  %s  ·  progress: %s\n", h.FQDN, hostStatus(h), h.Progress)
		if h.Agent != nil {
			fmt.Printf("agent %s · restic %s · %s %s · last seen %s · runs revision %d (current for this host: %d)\n",
				h.Agent.AgentVersion, h.Agent.ResticVersion, h.Agent.OSID, h.Agent.OSVersion, ago(h.Agent.LastSeenAt), h.Agent.AppliedRevision, h.DesiredRevision)
		}
		if h.ResticWarning != "" {
			fmt.Println("warning:", h.ResticWarning)
		}
		if h.AgentTooOld {
			fmt.Println("warning: the agent is older than this server accepts: upgrade holdfast-agent on the host (docs/install.md).\n" +
				"  It backs up on its cached config, and gets no config changes or commands meanwhile.")
		}
		if h.Disappeared > 0 {
			fmt.Printf("ALERT: %d snapshots disappeared from the repository without Holdfast deleting them.\n"+
				"  They can be recovered from S3's old versions until the bucket's lifecycle rule expires them.\n"+
				"  holdfast snapshot list %s --all shows them; holdfast host acknowledge %s clears this.\n", h.Disappeared, h.FQDN, h.FQDN)
		}
		if len(h.ForeignKeys) > 0 {
			fmt.Printf("ALERT: %d restic passwords were added to the repository outside Holdfast; whoever holds one can read every backup.\n"+
				"  Key IDs: %s. Remove the ones you did not add (restic key remove), then holdfast host acknowledge %s.\n",
				len(h.ForeignKeys), strings.Join(h.ForeignKeys, ", "), h.FQDN)
		}
		if h.NotMadeByHoldfast > 0 {
			fmt.Printf("warning: %d snapshots in the repository were not made by Holdfast; Holdfast leaves them alone\n", h.NotMadeByHoldfast)
		}
		if h.CatalogueError != "" {
			fmt.Println("warning: the snapshot list could not be read:", h.CatalogueError)
		}
		if h.Storage != nil {
			if len(h.Storage.Findings) == 0 {
				fmt.Printf("storage: every check passed at %s\n", h.Storage.CheckedAt.Local().Format("2006-01-02 15:04"))
			} else {
				fmt.Printf("storage: checks failed at %s (docs/storage.md):%s\n", h.Storage.CheckedAt.Local().Format("2006-01-02 15:04"), h.Storage.lines())
			}
			if h.Storage.ObjectLock != "" {
				fmt.Println("storage: the bucket's Object Lock is", h.Storage.ObjectLock)
			}
		}
		printOperating(h)
		w := table()
		fmt.Fprintln(w, "\nJOB\tLAST STATUS\tLAST SUCCESS\tNEXT RUN\tLONGEST GAP\tWORST-CASE LOSS")
		for _, j := range h.Jobs {
			st, last := "—", "—"
			if j.Status != nil {
				st, last = j.Status.LastStatus, ago(j.Status.LastSuccessAt)
			}
			next := "—"
			if len(j.NextRuns) > 0 {
				next = j.NextRuns[0].Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", j.ID, st, last, next, j.LongestGap, j.WorstCaseLoss)
		}
		return w.Flush()
	}})
	var add jobFlags
	var msg string
	addCmd := &cobra.Command{Use: "add <fqdn>", Short: "Add a host (a new revision), and get its 15-minute enrollment code", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		in := map[string]any{"fqdn": a[0], "message": msg}
		if add.id != "" {
			job, err := add.job()
			if err != nil {
				return err
			}
			in["job_id"], in["job"], in["accept"] = add.id, job, add.accept
		}
		var res struct {
			Revision   int64      `json:"revision"`
			Warning    string     `json:"warning"`
			Findings   []finding  `json:"findings"`
			Accepted   []string   `json:"accepted_codes"`
			Enrollment enrollment `json:"enrollment"`
		}
		raw, err := call("POST", "/api/v1/hosts", in, &res)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("added %s in revision %d; its storage passed every check\n%s", a[0], res.Revision, res.Enrollment.howTo())
		if len(res.Findings) > 0 {
			fmt.Println("\nchecks on job " + add.id + ":")
			printFindings(res.Findings, res.Accepted)
		}
		if res.Warning != "" {
			fmt.Println("\nwarning:", res.Warning)
		}
		return nil
	}}
	addCmd.Flags().StringVarP(&msg, "message", "m", "", "revision message")
	addCmd.Flags().StringVar(&add.id, "job", "", "also add this job (then the job flags apply)")
	add.register(addCmd)
	c.AddCommand(addCmd)
	c.AddCommand(&cobra.Command{Use: "acknowledge <fqdn>", Short: "Clear the red alerts for changes made outside Holdfast, after looking into them", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		var res struct{ Snapshots, Keys int }
		raw, err := call("POST", "/api/v1/hosts/"+url.PathEscape(a[0])+"/acknowledge", nil, &res)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("acknowledged on %s: %d disappeared snapshots (their record stays), %d repository keys added outside Holdfast (now accepted)\n", a[0], res.Snapshots, res.Keys)
		return nil
	}})
	var removeMsg string
	removeHost := &cobra.Command{Use: "remove <fqdn>", Short: "Remove a host (a new revision): its agent stops, its repository stays; adding it again brings it back",
		Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
			var res struct {
				Revision int64 `json:"revision"`
			}
			raw, err := call("DELETE", "/api/v1/hosts/"+url.PathEscape(a[0]), map[string]any{"message": removeMsg}, &res)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			fmt.Printf("removed in revision %d. Its agent gets an empty config and stops backing up; the host is then retired.\n"+
				"Its repository and snapshots stay, and its agent's certificate stops working.\n", res.Revision)
			return nil
		}}
	removeHost.Flags().StringVarP(&removeMsg, "message", "m", "", "revision message")
	c.AddCommand(removeHost)
	c.AddCommand(hostOperatingCmds()...)
	c.AddCommand(hostImportCmd())
	c.AddCommand(&cobra.Command{Use: "enroll-code <fqdn>", Short: "Issue a new 15-minute enrollment code (revokes the open one)", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		var e enrollment
		raw, err := call("POST", "/api/v1/hosts/"+url.PathEscape(a[0])+"/enrollment-code", nil, &e)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Print(e.howTo())
		return nil
	}})
	c.AddCommand(&cobra.Command{Use: "events <fqdn>", Short: "Recent runs on a host", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		var evs []struct {
			Kind, JobID, Status, SnapshotID, Error string
			Finished                               time.Time
		}
		raw, err := call("GET", "/api/v1/hosts/"+url.PathEscape(a[0])+"/events", nil, &evs)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		w := table()
		fmt.Fprintln(w, "FINISHED\tKIND\tJOB\tSTATUS\tSNAPSHOT\tERROR")
		for _, e := range evs {
			id := e.SnapshotID
			if len(id) > 8 {
				id = id[:8]
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Finished.Local().Format("2006-01-02 15:04:05"), e.Kind, e.JobID, e.Status, id, e.Error)
		}
		return w.Flush()
	}})
	return c
}

// jobFlags turn command-line flags into a job document. The server validates it.
type jobFlags struct {
	id, every, from, at, zone                  string
	daily, weekly, paths, excludes             []string
	spread                                     int
	noSpread                                   bool
	within, hourly, dly, wkly, monthly, yearly string
	keepLast                                   int
	accept                                     []string
	limitUpload                                int
	dump                                       string
}

// finding is one check that fired (spec §4.5).
type finding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// printFindings shows the checks that fired on a saved or checked job, marking the accepted warnings.
func printFindings(fs []finding, accepted []string) {
	for _, f := range fs {
		sev := f.Severity
		if sev == "warning" && slices.Contains(accepted, f.Code) {
			sev = "accepted warning"
		}
		fmt.Printf("  %s  %s: %s\n", f.Code, sev, f.Message)
	}
}

func (f *jobFlags) register(c *cobra.Command) {
	c.Flags().StringSliceVar(&f.paths, "path", nil, "path to back up (repeat)")
	c.Flags().StringSliceVar(&f.excludes, "exclude", nil, "restic exclude pattern (repeat)")
	c.Flags().StringSliceVar(&f.daily, "daily", nil, "daily at these times, such as 02:00,14:00")
	c.Flags().StringVar(&f.every, "every", "", "every N minutes or hours, such as 30m or 4h")
	c.Flags().StringVar(&f.from, "from", "00:00", "with --every: the start time each day")
	c.Flags().StringSliceVar(&f.weekly, "weekly", nil, "weekly on these days, such as mon,thu (with --at)")
	c.Flags().StringVar(&f.at, "at", "", "with --weekly: the time")
	c.Flags().IntVar(&f.spread, "spread", 0, "random start spread in minutes")
	c.Flags().BoolVar(&f.noSpread, "no-spread", false, "turn the spread off (the number is kept)")
	c.Flags().StringVar(&f.zone, "zone", "", "IANA time zone (default: the server's default zone)")
	c.Flags().StringVar(&f.within, "keep-within", "", "keep every backup for this long, such as 7d")
	c.Flags().StringVar(&f.hourly, "keep-hourly", "", "keep one per hour for this long, such as 48h")
	c.Flags().StringVar(&f.dly, "keep-daily", "", "keep one per day for this long, such as 14d")
	c.Flags().StringVar(&f.wkly, "keep-weekly", "", "keep one per week for this long, such as 8w")
	c.Flags().StringVar(&f.monthly, "keep-monthly", "", "keep one per month for this long, such as 12mo")
	c.Flags().StringVar(&f.yearly, "keep-yearly", "", "keep one per year for this long, such as 3y")
	c.Flags().IntVar(&f.keepLast, "keep-last", 3, "always keep the newest N")
	c.Flags().StringSliceVar(&f.accept, "accept", nil, "accept these warning codes, such as R102,R104 (errors cannot be accepted)")
	c.Flags().IntVar(&f.limitUpload, "limit-upload", 0, "cap the backup's upload, in KiB/s (restic --limit-upload); 0 for none")
	c.Flags().StringVar(&f.dump, "dump", "", "back up a dump the host's agent.yaml defines, by name, instead of paths (decision 037)")
}

func window(s string) (map[string]any, error) {
	if s == "" {
		return nil, nil
	}
	// "mo" first, so 12mo is months; a bare "m" is refused because it could mean minutes or months.
	for _, u := range []struct{ suffix, unit string }{{"mo", "months"}, {"h", "hours"}, {"d", "days"}, {"w", "weeks"}, {"y", "years"}} {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			if n, err := strconv.Atoi(num); err == nil {
				return map[string]any{"n": n, "unit": u.unit}, nil
			}
		}
	}
	return nil, fmt.Errorf("retention window %q: use a number and h, d, w, mo or y, such as 14d or 12mo", s)
}

func (f *jobFlags) job() (map[string]any, error) {
	sch := map[string]any{"spread": map[string]any{"enabled": f.spread > 0 && !f.noSpread, "minutes": f.spread}}
	switch {
	case len(f.daily) > 0:
		sch["daily"] = map[string]any{"times": f.daily}
	case f.every != "":
		unit := "hours"
		num := strings.TrimSuffix(f.every, "h")
		if strings.HasSuffix(f.every, "m") {
			unit, num = "minutes", strings.TrimSuffix(f.every, "m")
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("--every %q: use a number and m or h, such as 30m or 4h", f.every)
		}
		sch["every"] = map[string]any{"n": n, "unit": unit, "from": f.from}
	case len(f.weekly) > 0:
		sch["weekly"] = map[string]any{"days": f.weekly, "at": f.at}
	default:
		return nil, errors.New("give a schedule: --daily, --every or --weekly")
	}
	if f.zone != "" {
		sch["zone"] = f.zone
	}
	ret := map[string]any{"keep_last": f.keepLast}
	for name, v := range map[string]string{"within": f.within, "hourly": f.hourly, "daily": f.dly, "weekly": f.wkly, "monthly": f.monthly, "yearly": f.yearly} {
		w, err := window(v)
		if err != nil {
			return nil, err
		}
		if w != nil {
			ret[name] = w
		}
	}
	j := map[string]any{"engine": "restic", "paths": f.paths, "excludes": f.excludes, "schedule": sch, "retention": ret, "limit_upload_kib": f.limitUpload}
	if f.dump != "" {
		j["dump"] = f.dump
	}
	return j, nil
}

func jobCmd() *cobra.Command {
	c := &cobra.Command{Use: "job", Short: "Jobs"}
	var f jobFlags
	var msg string
	var base int64
	set := &cobra.Command{Use: "set <fqdn> <job>", Short: "Create or replace a job (a new revision)", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, a []string) error {
		job, err := f.job()
		if err != nil {
			return err
		}
		in := map[string]any{"message": msg, "job": job, "accept": f.accept}
		if cmd.Flags().Changed("base") {
			in["base_revision"] = base
		}
		var res struct {
			Revision int64     `json:"revision"`
			Findings []finding `json:"findings"`
			Accepted []string  `json:"accepted_codes"`
		}
		raw, err := call("PUT", "/api/v1/hosts/"+url.PathEscape(a[0])+"/jobs/"+url.PathEscape(a[1]), in, &res)
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
	set.Flags().StringVarP(&msg, "message", "m", "", "revision message")
	set.Flags().Int64Var(&base, "base", 0, "refuse the save if the newest revision is not this one")
	f.register(set)
	c.AddCommand(set)
	var cf jobFlags
	check := &cobra.Command{Use: "check <fqdn> <job>", Short: "Run the checks on a job without saving it; the flags are those of job set", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
		job, err := cf.job()
		if err != nil {
			return err
		}
		var res struct {
			Findings []finding `json:"findings"`
			Blocking []finding `json:"blocking"`
			Problems []struct {
				Path    string `json:"path"`
				Message string `json:"message"`
			} `json:"problems"`
			Gets struct {
				LongestGap    string `json:"longest_gap"`
				WorstCaseLoss string `json:"worst_case_loss"`
				StaleAfter    string `json:"stale_after"`
				RestorePoints int    `json:"restore_points"`
				Oldest        string `json:"oldest"`
			} `json:"gets"`
		}
		raw, err := call("POST", "/api/v1/hosts/"+url.PathEscape(a[0])+"/jobs/"+url.PathEscape(a[1])+"/check", map[string]any{"job": job, "accept": cf.accept}, &res)
		if err != nil {
			return err
		}
		refused := len(res.Blocking) > 0 || len(res.Problems) > 0
		if asJSON {
			printJSON(raw)
			if refused {
				return errors.New("job set would refuse this change")
			}
			return nil
		}
		for _, p := range res.Problems {
			fmt.Printf("  %s: %s\n", p.Path, p.Message)
		}
		if g := res.Gets; g.LongestGap != "" {
			fmt.Printf("what you get: about %d restore points, the oldest about %s old; worst-case loss %s; stale alert after %s\n",
				g.RestorePoints, g.Oldest, g.WorstCaseLoss, g.StaleAfter)
		}
		if len(res.Findings) == 0 && len(res.Problems) == 0 {
			fmt.Println("every check passed")
			return nil
		}
		if len(res.Findings) == 0 {
			return errors.New("job set would refuse this change")
		}
		blocked := map[string]bool{}
		for _, b := range res.Blocking {
			blocked[b.Code] = true
		}
		var accepted []string
		for _, f := range res.Findings {
			if f.Severity == "warning" && !blocked[f.Code] {
				accepted = append(accepted, f.Code)
			}
		}
		printFindings(res.Findings, accepted)
		if len(res.Blocking) > 0 {
			return errors.New("job set would refuse this change")
		}
		fmt.Println("job set would save this change")
		return nil
	}}
	cf.register(check)
	c.AddCommand(check, keepLongerCmd())
	var rmMsg string
	remove := &cobra.Command{Use: "remove <fqdn> <job>", Short: "Remove a job (a new revision); its snapshots stay", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
		var res struct {
			Revision int64 `json:"revision"`
		}
		raw, err := call("DELETE", "/api/v1/hosts/"+url.PathEscape(a[0])+"/jobs/"+url.PathEscape(a[1]), map[string]any{"message": rmMsg}, &res)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("removed in revision %d; its snapshots stay, and the daily retention no longer touches them\n", res.Revision)
		return nil
	}}
	remove.Flags().StringVarP(&rmMsg, "message", "m", "", "revision message")
	c.AddCommand(remove)
	c.AddCommand(&cobra.Command{Use: "run <fqdn> <job>", Short: "Back up now", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
		var res struct {
			Command string `json:"command"`
		}
		raw, err := call("POST", "/api/v1/hosts/"+url.PathEscape(a[0])+"/jobs/"+url.PathEscape(a[1])+"/run", nil, &res)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("queued (command %s); the agent picks it up on its next poll\n", res.Command)
		return nil
	}})
	return c
}

func revisionCmd() *cobra.Command {
	c := &cobra.Command{Use: "revision", Short: "Config history"}
	c.AddCommand(&cobra.Command{Use: "list", Short: "List revisions", RunE: func(*cobra.Command, []string) error {
		var revs []struct {
			Number         int64
			CreatedAt      time.Time `json:"created_at"`
			Actor, Message string
			AcceptedCodes  []string `json:"accepted_codes"`
		}
		raw, err := call("GET", "/api/v1/revisions", nil, &revs)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		w := table()
		fmt.Fprintln(w, "REVISION\tWHEN\tWHO\tACCEPTED\tMESSAGE")
		for _, r := range revs {
			acc := strings.Join(r.AcceptedCodes, ",")
			if acc == "" {
				acc = "—"
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", r.Number, r.CreatedAt.Local().Format("2006-01-02 15:04"), r.Actor, acc, r.Message)
		}
		return w.Flush()
	}})
	c.AddCommand(&cobra.Command{Use: "show <n>", Short: "Show a revision's full config", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		raw, err := call("GET", "/api/v1/revisions/"+a[0], nil, nil)
		if err != nil {
			return err
		}
		printJSON(raw)
		return nil
	}})
	c.AddCommand(&cobra.Command{Use: "diff <a> <b>", Short: "Compare two revisions", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
		var d struct {
			Changes []struct {
				Path     string
				From, To any
			}
		}
		raw, err := call("GET", "/api/v1/revisions/"+a[0]+"/diff/"+a[1], nil, &d)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		for _, ch := range d.Changes {
			switch {
			case ch.From == nil:
				fmt.Printf("+ %s = %v\n", ch.Path, ch.To)
			case ch.To == nil:
				fmt.Printf("- %s = %v\n", ch.Path, ch.From)
			default:
				fmt.Printf("~ %s: %v → %v\n", ch.Path, ch.From, ch.To)
			}
		}
		if len(d.Changes) == 0 {
			fmt.Println("no differences")
		}
		return nil
	}})
	return c
}

func auditCmd() *cobra.Command {
	var host string
	c := &cobra.Command{Use: "audit", Short: "The audit log", RunE: func(*cobra.Command, []string) error {
		var list []struct {
			At                         time.Time
			Actor, Action, FQDN, JobID string
			Revision                   int64
			Detail                     map[string]any
		}
		raw, err := call("GET", "/api/v1/audit?host="+url.QueryEscape(host), nil, &list)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		w := table()
		fmt.Fprintln(w, "WHEN\tWHO\tACTION\tHOST\tJOB\tDETAIL")
		for _, e := range list {
			d, _ := json.Marshal(e.Detail)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", e.At.Local().Format("2006-01-02 15:04:05"), e.Actor, e.Action, e.FQDN, e.JobID, d)
		}
		return w.Flush()
	}}
	c.Flags().StringVar(&host, "host", "", "only this host")
	return c
}

func checkCmd() *cobra.Command {
	return &cobra.Command{Use: "check", Short: "Test every host's storage now, and ask each agent to check its repository", RunE: func(*cobra.Command, []string) error {
		var res []struct{ FQDN, Status, Error string }
		raw, err := call("POST", "/api/v1/maintenance/check", nil, &res)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		w := table()
		fmt.Fprintln(w, "HOST\tSTATUS\tERROR")
		for _, r := range res {
			fmt.Fprintf(w, "%s\t%s\t%s\n", r.FQDN, r.Status, r.Error)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Println("\neach agent runs the repository check next; its result appears in holdfast host events <fqdn>")
		return nil
	}}
}

func escrowCmd() *cobra.Command {
	c := &cobra.Command{Use: "escrow", Short: "Escrow bundle (root on the server only)"}
	var to string
	export := &cobra.Command{Use: "export", Short: "Write every host's credentials to a 0600 file, to store offline", RunE: func(*cobra.Command, []string) error {
		if to == "" {
			return errors.New("--to is required")
		}
		f, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		raw, err := call("POST", "/api/v1/escrow/export", nil, nil)
		if err != nil {
			_ = os.Remove(to)
			return err
		}
		var v any
		_ = json.Unmarshal(raw, &v)
		b, _ := json.MarshalIndent(v, "", "  ")
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
		fmt.Printf("wrote %s (mode 0600). It holds every repository password in the clear: store it offline and delete this copy.\n", to)
		return nil
	}}
	export.Flags().StringVar(&to, "to", "", "file to create (it must not exist)")
	c.AddCommand(export)
	return c
}
