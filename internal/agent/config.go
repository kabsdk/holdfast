package agent

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/restic"
)

// Config is /etc/holdfast/agent.yaml, written by `holdfast-agent enroll`.
type Config struct {
	Server         string        `yaml:"server"`    // https://holdfast.example.internal
	FQDN           string        `yaml:"fqdn"`      // the host's identity
	CASHA256       string        `yaml:"ca_sha256"` // pins the server's certificate chain; empty: system roots
	StateDir       string        `yaml:"state_dir"`
	CredentialsDir string        `yaml:"credentials_dir"` // repo.pass, 0600, under /etc/holdfast
	ResticBin      string        `yaml:"restic_bin"`
	PollTimeout    time.Duration `yaml:"poll_timeout"`
	Tick           time.Duration `yaml:"tick"`       // scheduler resolution
	RetryLock      time.Duration `yaml:"retry_lock"` // how long a backup waits for maintenance's lock
	// Restore bounds what a restore may do on this host (decision 030, plan-m2 Q5). Only root here can
	// change them: the server cannot.
	Restore proto.RestoreBounds `yaml:"restore"`
	// Dumps are the commands a dump job may name (decision 037). Only root here defines them.
	Dumps map[string]Dump `yaml:"dumps,omitempty"`
}

// Dump is one command whose output a dump job backs up.
type Dump struct {
	Command  []string          `yaml:"command"`            // argv; the first is an absolute path
	User     string            `yaml:"user,omitempty"`     // run as this user; default root
	Filename string            `yaml:"filename,omitempty"` // the file's name in the snapshot; default <name>.dump
	Env      map[string]string `yaml:"env,omitempty"`      // on top of the clean PATH and LANG
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateDumps checks the dumps, and fills in the default file names.
func (c *Config) validateDumps() error {
	var errs []error
	for name, d := range c.Dumps {
		bad := func(f string, a ...any) {
			errs = append(errs, fmt.Errorf("dumps.%s: "+f, append([]any{name}, a...)...))
		}
		if err := config.ValidDumpName(name); err != nil {
			bad("%v", err)
		}
		if len(d.Command) == 0 || !filepath.IsAbs(d.Command[0]) {
			bad("command must be a list whose first item is an absolute path, such as [/usr/bin/pg_dumpall]")
		}
		if d.Filename == "" {
			d.Filename = name + ".dump"
		}
		if strings.ContainsAny(d.Filename, "/\x00") || d.Filename == "." || d.Filename == ".." {
			bad("filename %q must be a plain file name", d.Filename)
		}
		for k := range d.Env {
			if !envName.MatchString(k) {
				bad("env: %q is not a variable name", k)
			}
		}
		c.Dumps[name] = d
	}
	return errors.Join(errs...)
}

// DumpNames lists the dumps, sorted, as the agent reports them: names only, never commands.
func (c Config) DumpNames() []string {
	names := make([]string, 0, len(c.Dumps))
	for n := range c.Dumps {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// dumpCommand is what restic runs for a dump: the command with a clean environment, as its user. restic
// passes its own environment, the host's S3 key in it, to the command; env -i drops it (decision 037).
func dumpCommand(d Dump) []string {
	argv := []string{"/usr/bin/env", "-i", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	keys := make([]string, 0, len(d.Env))
	for k := range d.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, k+"="+d.Env[k])
	}
	if d.User != "" && d.User != "root" {
		argv = append(argv, "runuser", "-u", d.User, "--")
	}
	return append(argv, d.Command...)
}

// Defaults fills in what an install leaves out.
func (c *Config) Defaults() {
	if c.StateDir == "" {
		c.StateDir = "/var/lib/holdfast-agent"
	}
	if c.CredentialsDir == "" {
		c.CredentialsDir = "/etc/holdfast/credentials"
	}
	if c.PollTimeout <= 0 {
		c.PollTimeout = 25 * time.Second
	}
	if c.Tick <= 0 {
		c.Tick = time.Second
	}
	if c.RetryLock <= 0 {
		c.RetryLock = 15 * time.Minute
	}
	if c.Restore.Root == "" {
		c.Restore.Root = "/var/restore"
	}
	if c.ResticBin == "" {
		c.ResticBin = restic.DefaultBin // where the agent's package puts the pinned restic (decision 032)
	}
}

// LoadConfig reads the agent configuration strictly.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	c.Defaults()
	if c.Server == "" || c.FQDN == "" {
		return Config{}, errors.New("server and fqdn are required")
	}
	if err := c.validateDumps(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// WriteConfig writes the configuration with mode 0600.
func WriteConfig(path string, c Config) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, 0o600)
}
