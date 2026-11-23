package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// uiLoginCmd is root's way into the web UI (plan-m10 item 6): a one-time link from the root socket.
func uiLoginCmd() *cobra.Command {
	return &cobra.Command{Use: "ui-login", Short: "Root on the server: a one-time link into the web UI, valid 5 minutes, that logs you in as root, an admin on every host",
		Long: "As root on the server, print a link that opens the web UI logged in as local:root, an admin on every host, for 12\n" +
			"hours. It works once, within 5 minutes. It is the web UI's login on a server without an identity provider, and\n" +
			"the way in when the provider is down. It is audited, with the sudo user who asked for it.",
		Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
			var res struct {
				URL       string    `json:"url"`
				ExpiresAt time.Time `json:"expires_at"`
			}
			raw, err := call("POST", "/api/v1/ui-login", map[string]any{"sudo_user": os.Getenv("SUDO_USER")}, &res)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			fmt.Printf("Open this link in your browser before %s. It works once, and logs you in as root, an admin on\nevery host, for 12 hours:\n\n  %s\n",
				res.ExpiresAt.Local().Format("15:04"), res.URL)
			return nil
		}}
}
