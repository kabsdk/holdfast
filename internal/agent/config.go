package agent

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/kabsdk/holdfast/internal/proto"
)

// Config is /etc/holdfast/agent.yaml, written by `holdfast-agent enroll`.
type Config struct {
	Server         string        `yaml:"server"`    // https://holdfast.example.internal
	FQDN           string        `yaml:"fqdn"`      // the host's identity
	CASHA256       string        `yaml:"ca_sha256"` // pins the server's certificate chain; empty: system roots
	StateDir       string        `yaml:"state_dir"`
	CredentialsDir string        `yaml:"credentials_dir"` // repo.pass and s3.env, 0600, under /etc/holdfast
	ResticBin      string        `yaml:"restic_bin"`
	PollTimeout    time.Duration `yaml:"poll_timeout"`
	Tick           time.Duration `yaml:"tick"`       // scheduler resolution
	RetryLock      time.Duration `yaml:"retry_lock"` // how long a backup waits for maintenance's lock
	// Restore bounds what a restore may do on this host (decision 030, plan-m2 Q5). Only root here can
	// change them: the server cannot.
	Restore proto.RestoreBounds `yaml:"restore"`
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
