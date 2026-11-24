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

// PostgreSQL connections (decision 055): set centrally, their passwords sealed on the server and sent only to the
// agent. A password is read from a file, never from the command line.

type pgDatabase struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Owner string `json:"owner"`
}

type pgTestResult struct {
	ServerVersion string       `json:"server_version"`
	Client        string       `json:"client"`
	TLS           string       `json:"tls"`
	Databases     []pgDatabase `json:"databases"`
	RestoreUser   *struct {
		CreateDB bool     `json:"createdb"`
		ActsAs   []string `json:"acts_as"`
		Error    string   `json:"error"`
	} `json:"restore_user"`
}

type connectionView struct {
	Name               string   `json:"name"`
	URL                string   `json:"url"`
	User               string   `json:"user"`
	PasswordSet        bool     `json:"password_set"`
	RestoreUser        string   `json:"restore_user"`
	RestorePasswordSet bool     `json:"restore_password_set"`
	UsedBy             []string `json:"used_by"`
	LastTest           *struct {
		At      time.Time     `json:"at"`
		OK      bool          `json:"ok"`
		Command string        `json:"command"`
		Error   string        `json:"error"`
		Result  *pgTestResult `json:"result"`
	} `json:"last_test"`
}

func (c connectionView) print() {
	fmt.Printf("%s  %s  as %s", c.Name, c.URL, c.User)
	if c.PasswordSet {
		fmt.Print(" (password set)")
	}
	if c.RestoreUser != "" {
		fmt.Printf("  · restores as %s", c.RestoreUser)
	}
	fmt.Printf("\n  used by: %s\n", orNone(strings.Join(c.UsedBy, ", ")))
	t := c.LastTest
	switch {
	case t == nil:
		fmt.Println("  not tested yet")
	case !t.OK:
		fmt.Printf("  last test %s: FAILED: %s\n", ago(&t.At), t.Error)
	default:
		fmt.Printf("  last test %s: OK", ago(&t.At))
		if r := t.Result; r != nil {
			fmt.Printf(" · PostgreSQL %s · pg_dump %s", r.ServerVersion, r.Client)
			if r.TLS != "" {
				fmt.Printf(" · %s", r.TLS)
			}
			fmt.Println()
			w := table()
			fmt.Fprintln(w, "  DATABASE\tSIZE\tOWNER")
			for _, d := range r.Databases {
				fmt.Fprintf(w, "  %s\t%d MB\t%s\n", d.Name, d.Bytes/1000/1000, d.Owner)
			}
			_ = w.Flush()
			if ru := r.RestoreUser; ru != nil {
				if ru.Error != "" {
					fmt.Printf("  restore user: FAILED: %s\n", ru.Error)
				} else {
					fmt.Printf("  restore user: createdb %v, acts as %s\n", ru.CreateDB, orNone(strings.Join(ru.ActsAs, ", ")))
				}
			}
		} else {
			fmt.Println()
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// readSecretFile reads a password from a file, or from standard input for "-", without its trailing newline.
func readSecretFile(path string) (string, error) {
	var b []byte
	var err error
	if path == "-" {
		b, err = readAllStdin()
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

func readAllStdin() ([]byte, error) {
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return out, nil
			}
			return out, err
		}
	}
}

func connectionCmd() *cobra.Command {
	c := &cobra.Command{Use: "connection", Short: "PostgreSQL connections of an agent: where it connects, and as whom (passwords are sealed on the server)"}
	c.AddCommand(&cobra.Command{Use: "list <agent>", Short: "An agent's PostgreSQL connections, and what their last tests found", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, a []string) error {
			var list []connectionView
			raw, err := call("GET", "/api/v1/agents/"+url.PathEscape(a[0])+"/postgresql", nil, &list)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			if len(list) == 0 {
				fmt.Println("no PostgreSQL connection: add one with holdfast agent connection set")
			}
			for _, v := range list {
				v.print()
			}
			return nil
		}})
	var u, user, pwFile, ruser, rpwFile, msg string
	var noPW bool
	set := &cobra.Command{Use: "set <agent> <name>", Short: "Add or change a PostgreSQL connection (a new revision); the agent tests it at once",
		Long: "Add or change a PostgreSQL connection, as a new revision. The URL is libpq's, without a user, password or database,\n" +
			"such as postgresql://db.example.internal:5432/?sslmode=verify-full&sslrootcert=/etc/holdfast/pg/ca.crt, or\n" +
			"postgresql:///?host=/var/run/postgresql for a socket; paths in it are on the agent. Passwords are read from files\n" +
			"(- for standard input), never from the command line; a password not given keeps the one stored.",
		Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, a []string) error {
			in := map[string]any{"url": u, "user": user, "restore_user": ruser, "message": msg}
			if pwFile == "-" && rpwFile == "-" {
				return errors.New("only one password can come from standard input")
			}
			switch {
			case noPW:
				in["password"] = ""
			case pwFile != "":
				pw, err := readSecretFile(pwFile)
				if err != nil {
					return err
				}
				in["password"] = pw
			}
			if rpwFile != "" {
				pw, err := readSecretFile(rpwFile)
				if err != nil {
					return err
				}
				in["restore_password"] = pw
			}
			var res struct {
				Revision int64  `json:"revision"`
				Test     string `json:"test_command"`
			}
			raw, err := call("PUT", "/api/v1/agents/"+url.PathEscape(a[0])+"/postgresql/"+url.PathEscape(a[1]), in, &res)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			fmt.Printf("saved as revision %d; the agent tests it now (holdfast agent connection list %s shows the result)\n", res.Revision, a[0])
			return nil
		}}
	set.Flags().StringVar(&u, "url", "", "libpq's connection URI, without a user, password or database")
	set.Flags().StringVar(&user, "user", "", "the user to back up as: it reads every database (pg_read_all_data)")
	set.Flags().StringVar(&pwFile, "password-file", "", "a file holding the user's password (- for standard input)")
	set.Flags().BoolVar(&noPW, "no-password", false, "no password: peer authentication on a socket")
	set.Flags().StringVar(&ruser, "restore-user", "", "the user to restore as: it may create databases and act as their owners; without it, dumps restore as files only")
	set.Flags().StringVar(&rpwFile, "restore-password-file", "", "a file holding the restore user's password (- for standard input)")
	set.Flags().StringVarP(&msg, "message", "m", "", "revision message")
	c.AddCommand(set)
	var rmMsg string
	rm := &cobra.Command{Use: "remove <agent> <name>", Short: "Remove a connection no job uses, and its passwords (a new revision)", Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, a []string) error {
			var res struct {
				Revision int64 `json:"revision"`
			}
			raw, err := call("DELETE", "/api/v1/agents/"+url.PathEscape(a[0])+"/postgresql/"+url.PathEscape(a[1])+"?message="+url.QueryEscape(rmMsg), nil, &res)
			if err != nil {
				return err
			}
			if asJSON {
				printJSON(raw)
				return nil
			}
			fmt.Printf("removed in revision %d\n", res.Revision)
			return nil
		}}
	rm.Flags().StringVarP(&rmMsg, "message", "m", "", "revision message")
	c.AddCommand(rm)
	var wait time.Duration
	test := &cobra.Command{Use: "test <agent> <name>", Short: "Have the agent connect, as it would to back up, and show what it finds", Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, a []string) error {
			var res struct {
				Command string `json:"command"`
			}
			raw, err := call("POST", "/api/v1/agents/"+url.PathEscape(a[0])+"/postgresql/"+url.PathEscape(a[1])+"/test", nil, &res)
			if err != nil {
				return err
			}
			if asJSON && wait == 0 {
				printJSON(raw)
				return nil
			}
			deadline := time.Now().Add(wait)
			for time.Now().Before(deadline) {
				var list []json.RawMessage
				if _, err := call("GET", "/api/v1/agents/"+url.PathEscape(a[0])+"/postgresql", nil, &list); err != nil {
					return err
				}
				for _, raw := range list {
					var v connectionView
					if err := json.Unmarshal(raw, &v); err != nil {
						return err
					}
					if v.Name == a[1] && v.LastTest != nil && v.LastTest.Command == res.Command {
						if asJSON {
							printJSON(raw)
						} else {
							v.print()
						}
						if !v.LastTest.OK {
							return errors.New("the test failed")
						}
						return nil
					}
				}
				time.Sleep(time.Second)
			}
			return fmt.Errorf("queued (command %s), but the agent has not answered within %s: is it online?", res.Command, wait)
		}}
	test.Flags().DurationVar(&wait, "wait", time.Minute, "how long to wait for the agent's answer")
	c.AddCommand(test)
	return c
}
