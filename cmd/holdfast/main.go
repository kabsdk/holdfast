// holdfast is the CLI: a thin client of the /api/v1 API (decision 028). It turns flags into API
// requests and API answers into tables; every check and rule is on the server.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

var (
	socketPath string
	asJSON     bool
)

func main() {
	root := &cobra.Command{Use: "holdfast", Short: "Holdfast command line (a client of the Holdfast API)", SilenceUsage: true}
	root.PersistentFlags().StringVar(&socketPath, "socket", envOr("HOLDFAST_SOCKET", "/run/holdfast/api.sock"), "API socket of holdfast-server")
	root.PersistentFlags().BoolVar(&asJSON, "json", false, "print the API's JSON answer")
	root.AddCommand(statusCmd(), hostCmd(), jobCmd(), snapshotCmd(), revisionCmd(), auditCmd(), checkCmd(), escrowCmd())
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

func client() *http.Client {
	return &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}}
}

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
}

func (r *storageReport) lines() string {
	var b strings.Builder
	for _, f := range r.Findings {
		b.WriteString("\n  " + f.Code + "  " + f.Message)
	}
	return b.String()
}

// readS3Key reads a key file of AWS_ACCESS_KEY_ID= and AWS_SECRET_ACCESS_KEY= lines, or standard input
// for "-". The server validates the key and tests it; the secret never goes on a command line.
func readS3Key(path string) (map[string]string, error) {
	var r io.Reader = os.Stdin
	if path != "-" {
		fi, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("%s has mode %04o: a key file must be readable by its owner only", path, fi.Mode().Perm())
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	key := map[string]string{"access_key_id": "", "secret_access_key": ""}
	b, err := io.ReadAll(io.LimitReader(r, 1<<16))
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(strings.TrimPrefix(name, "export ")) {
		case "AWS_ACCESS_KEY_ID":
			key["access_key_id"] = value
		case "AWS_SECRET_ACCESS_KEY":
			key["secret_access_key"] = value
		}
	}
	return key, nil
}

// call sends a request and decodes the answer into out. It returns the raw body for --json.
func call(method, path string, in, out any) ([]byte, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://holdfast"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach holdfast-server on %s: %w", socketPath, err)
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
				msg = "the S3 key does not meet the storage contract (docs/storage.md):" + e.Storage.lines()
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
	Snapshots         int            `json:"snapshots"`
	NotMadeByHoldfast int            `json:"not_made_by_holdfast"`
	Disappeared       int            `json:"disappeared"`
	CatalogueError    string         `json:"catalogue_error"`
	Agent             *struct {
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
		if h.Disappeared > 0 {
			fmt.Printf("ALERT: %d snapshots disappeared from the repository without Holdfast deleting them.\n"+
				"  They can be recovered from S3's old versions until the bucket's lifecycle rule expires them.\n"+
				"  holdfast snapshot list %s --all shows them; holdfast host acknowledge %s clears this.\n", h.Disappeared, h.FQDN, h.FQDN)
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
		}
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
	var msg, keyFile string
	addCmd := &cobra.Command{Use: "add <fqdn>", Short: "Add a host (a new revision), and get its 15-minute enrollment code", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		in := map[string]any{"fqdn": a[0], "message": msg}
		if keyFile != "" {
			key, err := readS3Key(keyFile)
			if err != nil {
				return err
			}
			in["s3_key"] = key
		}
		if add.id != "" {
			job, err := add.job()
			if err != nil {
				return err
			}
			in["job_id"], in["job"] = add.id, job
		}
		var res struct {
			Revision   int64  `json:"revision"`
			Warning    string `json:"warning"`
			Enrollment struct {
				Code           string    `json:"code"`
				ExpiresAt      time.Time `json:"expires_at"`
				InstallCommand string    `json:"install_command"`
			} `json:"enrollment"`
		}
		raw, err := call("POST", "/api/v1/hosts", in, &res)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("added %s in revision %d; its S3 key passed every storage check\nenrollment code %s, valid until %s\n\nrun on the host, as root:\n  %s\n",
			a[0], res.Revision, res.Enrollment.Code, res.Enrollment.ExpiresAt.Local().Format("15:04"), res.Enrollment.InstallCommand)
		if res.Warning != "" {
			fmt.Println("\nwarning:", res.Warning)
		}
		return nil
	}}
	addCmd.Flags().StringVar(&keyFile, "s3-key-file", "", "the host's S3 key: a root-only file of AWS_ACCESS_KEY_ID= and AWS_SECRET_ACCESS_KEY= lines, or - for stdin (docs/storage.md)")
	addCmd.Flags().StringVarP(&msg, "message", "m", "", "revision message")
	addCmd.Flags().StringVar(&add.id, "job", "", "also add this job (then the job flags apply)")
	add.register(addCmd)
	c.AddCommand(addCmd)
	var setKeyFile, setKeyMsg string
	setKey := &cobra.Command{Use: "set-key <fqdn>", Short: "Replace a host's S3 key; the key is tested first, and a key that fails is refused", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		if setKeyFile == "" {
			return errors.New("--s3-key-file is required")
		}
		key, err := readS3Key(setKeyFile)
		if err != nil {
			return err
		}
		var res struct {
			CredentialsVersion int `json:"credentials_version"`
		}
		raw, err := call("PUT", "/api/v1/hosts/"+url.PathEscape(a[0])+"/s3-key", map[string]any{"s3_key": key, "message": setKeyMsg}, &res)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("the new S3 key of %s passed every storage check and is credentials version %d; the agent gets it on its next poll\n"+
			"export a new escrow bundle: the last one has the old key\n", a[0], res.CredentialsVersion)
		return nil
	}}
	setKey.Flags().StringVar(&setKeyFile, "s3-key-file", "", "a root-only file of AWS_ACCESS_KEY_ID= and AWS_SECRET_ACCESS_KEY= lines, or - for stdin")
	setKey.Flags().StringVarP(&setKeyMsg, "message", "m", "", "why the key is replaced, for the audit log")
	c.AddCommand(setKey)
	c.AddCommand(&cobra.Command{Use: "acknowledge <fqdn>", Short: "Clear the alert for snapshots that disappeared (the record of them stays)", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		var res struct{ Acknowledged int }
		raw, err := call("POST", "/api/v1/hosts/"+url.PathEscape(a[0])+"/snapshots/acknowledge", nil, &res)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("acknowledged %d disappeared snapshots of %s\n", res.Acknowledged, a[0])
		return nil
	}})
	c.AddCommand(&cobra.Command{Use: "enroll-code <fqdn>", Short: "Issue a new 15-minute enrollment code (revokes the open one)", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		var e struct {
			Code           string    `json:"code"`
			ExpiresAt      time.Time `json:"expires_at"`
			InstallCommand string    `json:"install_command"`
		}
		raw, err := call("POST", "/api/v1/hosts/"+url.PathEscape(a[0])+"/enrollment-code", nil, &e)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("enrollment code %s, valid until %s\n  %s\n", e.Code, e.ExpiresAt.Local().Format("15:04"), e.InstallCommand)
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
	return map[string]any{"engine": "restic", "paths": f.paths, "excludes": f.excludes, "schedule": sch, "retention": ret}, nil
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
		in := map[string]any{"message": msg, "job": job}
		if cmd.Flags().Changed("base") {
			in["base_revision"] = base
		}
		var res struct {
			Revision int64 `json:"revision"`
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
		return nil
	}}
	set.Flags().StringVarP(&msg, "message", "m", "", "revision message")
	set.Flags().Int64Var(&base, "base", 0, "refuse the save if the newest revision is not this one")
	f.register(set)
	c.AddCommand(set)
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
		fmt.Fprintln(w, "REVISION\tWHEN\tWHO\tMESSAGE")
		for _, r := range revs {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", r.Number, r.CreatedAt.Local().Format("2006-01-02 15:04"), r.Actor, r.Message)
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
		fmt.Printf("wrote %s (mode 0600). It holds every repository password and S3 key in the clear: store it offline and delete this copy.\n", to)
		return nil
	}}
	export.Flags().StringVar(&to, "to", "", "file to create (it must not exist)")
	c.AddCommand(export)
	return c
}
