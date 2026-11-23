package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// notifyCmd is the server's own alerts (plan-m10 item 2).
func notifyCmd() *cobra.Command {
	c := &cobra.Command{Use: "notify", Short: "The server's own alerts: mail, a webhook, and a heartbeat (notify: in server.yaml)"}
	c.AddCommand(&cobra.Command{Use: "test", Short: "Send a test alert by every channel that is set, and say how each went (admin)", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			var res struct {
				Mail      string `json:"mail"`
				Webhook   string `json:"webhook"`
				Heartbeat string `json:"heartbeat"`
			}
			raw, err := call("POST", "/api/v1/notify/test", nil, &res)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			for _, ch := range []struct{ name, outcome string }{{"mail", res.Mail}, {"webhook", res.Webhook}, {"heartbeat", res.Heartbeat}} {
				if ch.outcome != "" {
					fmt.Printf("%-9s %s\n", ch.name, ch.outcome)
				}
			}
			return nil
		}})
	return c
}
