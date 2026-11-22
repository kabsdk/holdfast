package main

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Operating Holdfast (plan-m5): rotating a compromised host's password, and decommission. The checks and
// the wait are on the server; the CLI only asks.

func hostOperatingCmds() []*cobra.Command {
	var rotateMsg string
	rotate := &cobra.Command{Use: "rotate <fqdn>", Short: "After a compromise: a new repository password, the agent's token revoked, and a new enrollment code (docs/runbooks/compromised-host.md)",
		Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
			var res struct {
				CredentialsVersion int      `json:"credentials_version"`
				NewKey             string   `json:"new_key"`
				RemovedKeys        []string `json:"removed_keys"`
				Escrowed           bool     `json:"escrowed"`
				Warning            string   `json:"warning"`
				Enrollment         struct {
					Code           string    `json:"code"`
					ExpiresAt      time.Time `json:"expires_at"`
					InstallCommand string    `json:"install_command"`
				} `json:"enrollment"`
			}
			raw, err := call("POST", "/api/v1/hosts/"+url.PathEscape(a[0])+"/rotate", map[string]any{"message": rotateMsg}, &res)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			fmt.Printf("%s has a new repository password (key %s, credentials version %d); removed key %s.\n"+
				"Its agent's token is revoked: the host gets nothing more until it enrolls again.\n",
				a[0], res.NewKey, res.CredentialsVersion, strings.Join(res.RemovedKeys, ", "))
			if res.Escrowed {
				fmt.Println("The escrow file in the bucket holds the new password.")
			}
			fmt.Printf("\nOnce the host is clean or rebuilt, enroll it with code %s (valid until %s), as root on the host:\n  %s\n",
				res.Enrollment.Code, res.Enrollment.ExpiresAt.Local().Format("15:04"), res.Enrollment.InstallCommand)
			fmt.Printf("\nAlso make the host a new S3 key and give it with holdfast host set-key %s; the old key may be known too.\n", a[0])
			if res.Warning != "" {
				fmt.Println("\nwarning:", res.Warning)
			}
			return nil
		}}
	rotate.Flags().StringVarP(&rotateMsg, "message", "m", "", "why, for the audit log")

	var confirm, msg string
	var cancel bool
	decommission := &cobra.Command{Use: "decommission <fqdn>", Short: "Admin: delete a removed host's repository after 7 days (--cancel stops it); needs --confirm <fqdn>",
		Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
			path := "/api/v1/hosts/" + url.PathEscape(a[0]) + "/decommission"
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
				"Until then, holdfast host decommission %s --cancel stops it. Afterwards, S3 keeps the old versions until\n"+
				"the bucket's lifecycle rule expires them; holdfast host show %s then says when the data is really gone.\n",
				a[0], d.DueAt.Local().Format("2006-01-02 15:04"), a[0], a[0])
			return nil
		}}
	decommission.Flags().StringVar(&confirm, "confirm", "", "the host's FQDN, typed to confirm")
	decommission.Flags().StringVarP(&msg, "message", "m", "", "why, for the audit log")
	decommission.Flags().BoolVar(&cancel, "cancel", false, "cancel a decommission that is still waiting")
	return []*cobra.Command{rotate, decommission}
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
			fmt.Printf("DECOMMISSION: the repository is deleted on %s, as %s asked; holdfast host decommission %s --cancel stops it\n",
				d.DueAt.Local().Format("2006-01-02 15:04"), d.RequestedBy, h.FQDN)
			if d.Error != "" {
				fmt.Println("  the last attempt failed:", d.Error)
			}
		}
	}
	if h.RepoKeyRetiring != "" {
		fmt.Printf("ALERT: the old repository password still opens the repository (key %s): run holdfast host rotate %s again\n", h.RepoKeyRetiring, h.FQDN)
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
