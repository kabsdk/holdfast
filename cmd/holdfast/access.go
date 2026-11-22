package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The access commands (M4): the role bindings and the sessions, for admins on every host.

type binding struct {
	Group string   `json:"group"`
	Role  string   `json:"role"`
	Hosts []string `json:"hosts"`
}

func accessCmd() *cobra.Command {
	c := &cobra.Command{Use: "access", Short: "Role bindings and sessions (admins on every host)"}
	c.AddCommand(&cobra.Command{Use: "list", Short: "The role bindings in force", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		var v struct {
			Revision int64     `json:"revision"`
			Bindings []binding `json:"bindings"`
		}
		raw, err := call("GET", "/api/v1/access", nil, &v)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("revision %d\n", v.Revision)
		w := table()
		fmt.Fprintln(w, "GROUP\tROLE\tHOSTS")
		for _, b := range v.Bindings {
			fmt.Fprintf(w, "%s\t%s\t%s\n", b.Group, b.Role, strings.Join(b.Hosts, " "))
		}
		return w.Flush()
	}})
	var file, msg string
	var base int64
	set := &cobra.Command{Use: "set --file <bindings.json>", Short: "Replace the role bindings with the list in a file (a new revision)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return errors.New("--file is required: a JSON list of {group, role, hosts}, such as holdfast access list --json prints under bindings")
			}
			b, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			var list []binding
			if err := json.Unmarshal(b, &list); err != nil {
				var wrapped struct {
					Bindings []binding `json:"bindings"`
				}
				if json.Unmarshal(b, &wrapped) != nil || wrapped.Bindings == nil {
					return fmt.Errorf("%s: %w", file, err)
				}
				list = wrapped.Bindings
			}
			in := map[string]any{"bindings": list, "message": msg}
			if cmd.Flags().Changed("base") {
				in["base_revision"] = base
			}
			var res struct {
				Revision int64 `json:"revision"`
			}
			raw, err := call("PUT", "/api/v1/access", in, &res)
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
	set.Flags().StringVar(&file, "file", "", "a JSON file with the bindings")
	set.Flags().StringVarP(&msg, "message", "m", "", "revision message")
	set.Flags().Int64Var(&base, "base", 0, "refuse the save if the newest revision is not this one")
	c.AddCommand(set)
	c.AddCommand(&cobra.Command{Use: "sessions", Short: "Who is logged in", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		var list []struct {
			ID         string    `json:"id"`
			Kind       string    `json:"kind"`
			Username   string    `json:"username"`
			Groups     []string  `json:"groups"`
			CreatedAt  time.Time `json:"created_at"`
			ExpiresAt  time.Time `json:"expires_at"`
			LastUsedAt time.Time `json:"last_used_at"`
		}
		raw, err := call("GET", "/api/v1/sessions", nil, &list)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		w := table()
		fmt.Fprintln(w, "USER\tKIND\tGROUPS\tSINCE\tLAST USED\tENDS")
		for _, s := range list {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Username, s.Kind, strings.Join(s.Groups, ","), s.CreatedAt.Local().Format("2006-01-02 15:04"),
				s.LastUsedAt.Local().Format("15:04"), s.ExpiresAt.Local().Format("2006-01-02 15:04"))
		}
		return w.Flush()
	}})
	c.AddCommand(&cobra.Command{Use: "end-sessions <user>", Short: "End every session of a user at once, such as after removing them from a group", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			var res struct {
				Ended int `json:"ended"`
			}
			raw, err := call("DELETE", "/api/v1/sessions/"+url.PathEscape(a[0]), nil, &res)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			fmt.Printf("ended %d sessions of %s\n", res.Ended, a[0])
			return nil
		}})
	return c
}
