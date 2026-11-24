package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Where the CLI sends its requests (decision 034):
//   - after `holdfast login --server <url>`, to that server over HTTPS, with the session token it got;
//   - otherwise, or with --socket, to the server's root-only socket, where root is admin on every host.

// savedLogin is the CLI's session, in a file only its owner can read.
type savedLogin struct {
	Server    string    `json:"server"`
	Token     string    `json:"token"`
	Username  string    `json:"username"`
	ExpiresAt time.Time `json:"expires_at"`
	CAFile    string    `json:"ca_file,omitempty"`
}

func loginPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "holdfast", "login.json")
}

func readLogin() (*savedLogin, error) {
	b, err := os.ReadFile(loginPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var l savedLogin
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("%s: %w", loginPath(), err)
	}
	return &l, nil
}

func writeLogin(l savedLogin) error {
	p := loginPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(l, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// target is where requests go.
type target struct {
	base   string // http://holdfast on the socket, or the server's URL
	client *http.Client
	token  string
}

var serverURL string // --server

func httpsClient(caFile string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no certificate", caFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Timeout: 10 * time.Minute, Transport: tr}, nil
}

func socketTarget() target {
	return target{base: "http://holdfast", client: &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}}}
}

// resolveTarget picks the socket or the saved login.
func resolveTarget(cmd *cobra.Command) (target, error) {
	if cmd.Flags().Changed("socket") {
		return socketTarget(), nil
	}
	l, err := readLogin()
	if err != nil {
		return target{}, err
	}
	if l == nil {
		if serverURL != "" {
			return target{}, fmt.Errorf("not logged in to %s: run holdfast login --server %s", serverURL, serverURL)
		}
		return socketTarget(), nil
	}
	if serverURL != "" && strings.TrimSuffix(serverURL, "/") != l.Server {
		return target{}, fmt.Errorf("logged in to %s, not %s: run holdfast login --server %s", l.Server, serverURL, serverURL)
	}
	c, err := httpsClient(l.CAFile)
	if err != nil {
		return target{}, err
	}
	return target{base: l.Server, client: c, token: l.Token}, nil
}

var current target

func (t target) do(method, path string, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, t.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		if t.token == "" {
			return nil, fmt.Errorf("cannot reach holdfast-server on %s: %w", socketPath, err)
		}
		return nil, fmt.Errorf("cannot reach %s: %w", t.base, err)
	}
	if resp.StatusCode == http.StatusUnauthorized && t.token != "" {
		resp.Body.Close()
		return nil, fmt.Errorf("your session on %s has ended: run holdfast login --server %s", t.base, t.base)
	}
	return resp, nil
}

func loginCmd() *cobra.Command {
	var caFile string
	c := &cobra.Command{Use: "login", Short: "Log in to a Holdfast server through its OIDC provider (a device code)", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		if serverURL == "" {
			return errors.New("give the server's URL: holdfast login --server https://holdfast.example.com")
		}
		base := strings.TrimSuffix(serverURL, "/")
		cl, err := httpsClient(caFile)
		if err != nil {
			return err
		}
		t := target{base: base, client: cl}
		var start struct {
			Request                 string    `json:"request"`
			UserCode                string    `json:"user_code"`
			VerificationURI         string    `json:"verification_uri"`
			VerificationURIComplete string    `json:"verification_uri_complete"`
			Interval                int       `json:"interval"`
			ExpiresAt               time.Time `json:"expires_at"`
		}
		if err := t.json("POST", "/auth/device", nil, &start, http.StatusOK); err != nil {
			return err
		}
		where := start.VerificationURI
		if start.VerificationURIComplete != "" {
			where = start.VerificationURIComplete
		}
		fmt.Printf("Open %s\nand check that it shows the code %s. Waiting until %s…\n", where, start.UserCode, start.ExpiresAt.Local().Format("15:04"))
		for time.Now().Before(start.ExpiresAt) {
			time.Sleep(time.Duration(max(start.Interval, 1)) * time.Second)
			resp, err := t.do("POST", "/auth/device/token", map[string]string{"request": start.Request})
			if err != nil {
				return err
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusAccepted:
				continue
			case http.StatusOK:
				var tok struct {
					Token     string    `json:"token"`
					Username  string    `json:"username"`
					ExpiresAt time.Time `json:"expires_at"`
				}
				if err := json.Unmarshal(b, &tok); err != nil {
					return err
				}
				if err := writeLogin(savedLogin{Server: base, Token: tok.Token, Username: tok.Username, ExpiresAt: tok.ExpiresAt, CAFile: caFile}); err != nil {
					return err
				}
				fmt.Printf("logged in to %s as %s, until %s\n", base, tok.Username, tok.ExpiresAt.Local().Format("2006-01-02 15:04"))
				return nil
			default:
				return errors.New(errorText(resp.Status, b))
			}
		}
		return errors.New("the code expired before the login was confirmed; run holdfast login again")
	}}
	c.Flags().StringVar(&caFile, "ca-file", "", "a CA certificate to trust for the server, if the system does not")
	return c
}

func logoutCmd() *cobra.Command {
	return &cobra.Command{Use: "logout", Short: "End your session on the server, and forget it here", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		l, err := readLogin()
		if err != nil || l == nil {
			return err
		}
		if cl, err := httpsClient(l.CAFile); err == nil {
			if resp, err := (target{base: l.Server, client: cl, token: l.Token}).do("POST", "/auth/logout", nil); err == nil {
				resp.Body.Close()
			}
		}
		if err := os.Remove(loginPath()); err != nil {
			return err
		}
		fmt.Printf("logged out of %s\n", l.Server)
		return nil
	}}
}

func whoamiCmd() *cobra.Command {
	return &cobra.Command{Use: "whoami", Short: "Who you are, and what you may do on which host", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		var me struct {
			Name            string              `json:"name"`
			Source          string              `json:"source"`
			Groups          []string            `json:"groups"`
			Everywhere      bool                `json:"everywhere"`
			AdminEverywhere bool                `json:"admin_everywhere"`
			AddHosts        bool                `json:"add_hosts"`
			Hosts           map[string][]string `json:"hosts"`
		}
		raw, err := call("GET", "/api/v1/me", nil, &me)
		if err != nil {
			return err
		}
		if asJSON {
			printJSON(raw)
			return nil
		}
		fmt.Printf("%s (%s), groups: %s\n", me.Name, me.Source, strings.Join(me.Groups, ", "))
		switch {
		case me.Everywhere:
			fmt.Println("admin on every host (root on the server, or auth.bootstrap_admins)")
		case me.AdminEverywhere:
			fmt.Println("admin on every host")
		}
		names := make([]string, 0, len(me.Hosts))
		for h := range me.Hosts {
			names = append(names, h)
		}
		sort.Strings(names)
		w := table()
		fmt.Fprintln(w, "AGENT\tYOU MAY")
		for _, h := range names {
			fmt.Fprintf(w, "%s\t%s\n", h, strings.Join(me.Hosts[h], " "))
		}
		return w.Flush()
	}}
}

// json does a request that must answer want, and decodes the answer.
func (t target) json(method, path string, in, out any, want int) error {
	resp, err := t.do(method, path, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		return errors.New(errorText(resp.Status, b))
	}
	return json.Unmarshal(b, out)
}

func errorText(status string, body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	return status + ": " + strings.TrimSpace(string(body))
}
