package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// Off-site copies (decisions 027 and 039). On holdfast-server: the vaults that copy its hosts, and their copies. On
// holdfast-vault (holdfast --vault): Exclude, Include and Match primary.

// vaultSocket is holdfast-vault's API socket, which --vault picks.
const vaultSocket = "/run/holdfast-vault/api.sock"

func vaultCmd() *cobra.Command {
	c := &cobra.Command{Use: "vault", Short: "The vaults that copy this site's hosts to another site (admin)"}
	var keyFile, msg string
	add := &cobra.Command{Use: "add <name>", Short: "Register a vault, with a key that may only read repos/ here; prints its token once",
		Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
			if keyFile == "" {
				return errors.New("--s3-key-file is required: a key that may only read repos/ in this site's bucket (docs/storage.md)")
			}
			key, err := readS3Key(keyFile)
			if err != nil {
				return err
			}
			var res struct {
				Name  string `json:"name"`
				Token string `json:"token"`
			}
			raw, err := call("POST", "/api/v1/vaults", map[string]any{"name": a[0], "s3_key": key, "message": msg}, &res)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			fmt.Printf("vault %s is registered. Its token, shown only now:\n\n  %s\n\n", res.Name, res.Token)
			fmt.Println("Put it alone in the vault's source.token_file (root-only, mode 0600); see docs/install.md.")
			return nil
		}}
	add.Flags().StringVar(&keyFile, "s3-key-file", "", "the vault's read-only key, as for host add, or - for stdin")
	add.Flags().StringVarP(&msg, "message", "m", "", "why, for the audit log")
	c.AddCommand(add)
	c.AddCommand(&cobra.Command{Use: "list", Short: "List the vaults, with what they copy", RunE: func(*cobra.Command, []string) error {
		var vs []struct {
			Name       string     `json:"name"`
			LastSeenAt *time.Time `json:"last_seen_at"`
			Hosts      int        `json:"hosts"`
			Pending    int        `json:"pending"`
			CopyErrors int        `json:"copy_errors"`
		}
		raw, err := call("GET", "/api/v1/vaults", nil, &vs)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "VAULT\tLAST SEEN\tHOSTS\tWAITING\tCOPY ERRORS")
		for _, v := range vs {
			seen := "never"
			if v.LastSeenAt != nil {
				seen = v.LastSeenAt.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\n", v.Name, seen, v.Hosts, v.Pending, v.CopyErrors)
		}
		return w.Flush()
	}})
	var rmMsg string
	remove := &cobra.Command{Use: "remove <name>", Short: "Forget a vault: its token stops working. Its copies stay in its site",
		Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
			raw, err := call("DELETE", "/api/v1/vaults/"+url.PathEscape(a[0]), map[string]any{"message": rmMsg}, nil)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			fmt.Printf("vault %s removed; this site no longer holds snapshots back for it\n", a[0])
			return nil
		}}
	remove.Flags().StringVarP(&rmMsg, "message", "m", "", "why, for the audit log")
	c.AddCommand(remove)
	return c
}

func offsiteCmd() *cobra.Command {
	c := &cobra.Command{Use: "offsite", Short: "Off-site copies: what the vaults hold of a host, and, on the vault, its settings"}
	c.AddCommand(&cobra.Command{Use: "copies <fqdn>", Short: "The copies each vault holds of a host, as it last reported (for restore --from-vault)",
		Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
			var vs []struct {
				Vault      string     `json:"vault"`
				ReportedAt *time.Time `json:"reported_at"`
				Copies     []struct {
					ID       string    `json:"id"`
					Original string    `json:"original"`
					Time     time.Time `json:"time"`
					JobID    string    `json:"job_id"`
				} `json:"copies"`
			}
			raw, err := call("GET", "/api/v1/hosts/"+url.PathEscape(a[0])+"/offsite", nil, &vs)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "VAULT\tCOPY\tORIGINAL\tTIME\tJOB")
			for _, v := range vs {
				for _, cp := range v.Copies {
					fmt.Fprintf(w, "%s\t%.8s\t%.8s\t%s\t%s\n", v.Vault, cp.ID, cp.Original, cp.Time.Local().Format("2006-01-02 15:04"), cp.JobID)
				}
			}
			return w.Flush()
		}})
	c.AddCommand(excludeCmd(true), excludeCmd(false))
	var msg string
	var accept []string
	match := &cobra.Command{Use: "match-primary <fqdn> <job>", Short: "On the vault: set a job's off-site retention to its primary retention again",
		Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
			var res struct {
				Revision int64     `json:"revision"`
				Findings []finding `json:"findings"`
				Accepted []string  `json:"accepted_codes"`
			}
			raw, err := call("POST", "/api/v1/hosts/"+url.PathEscape(a[0])+"/jobs/"+url.PathEscape(a[1])+"/match-primary",
				map[string]any{"message": msg, "accept": accept}, &res)
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
	match.Flags().StringVarP(&msg, "message", "m", "", "revision message")
	match.Flags().StringSliceVar(&accept, "accept", nil, "accept these warning codes (R104 when primary keeps less)")
	c.AddCommand(match)
	return c
}

// excludeCmd is Exclude, or Include.
func excludeCmd(exclude bool) *cobra.Command {
	use, short := "include <fqdn> <job>", "On the vault: copy an excluded job again"
	if exclude {
		use, short = "exclude <fqdn> <job>", "On the vault: stop copying a job; its copies stay under its off-site retention"
	}
	var msg string
	c := &cobra.Command{Use: use, Short: short, Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
		path := "/api/v1/hosts/" + url.PathEscape(a[0]) + "/jobs/" + url.PathEscape(a[1]) + "/include"
		if exclude {
			path = "/api/v1/hosts/" + url.PathEscape(a[0]) + "/jobs/" + url.PathEscape(a[1]) + "/exclude"
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
		fmt.Printf("revision %d\n", res.Revision)
		return nil
	}}
	c.Flags().StringVarP(&msg, "message", "m", "", "revision message")
	return c
}
