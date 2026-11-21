package server

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/kabsdk/holdfast/internal/restic"
)

// Settings is /etc/holdfast/server.yaml. Changing it needs root on the server; a reload is audited.
// auth.* is defined by spec §6.4 and is acted on from M4; the rest is
// operational (plan-m1 §9).
type Settings struct {
	Listen             string `yaml:"listen"`     // agent protocol, /metrics and /healthz, such as ":8443"
	APISocket          string `yaml:"api_socket"` // /api/v1 for local root (M1); the directory must be 0700
	PublicURL          string `yaml:"public_url"` // the URL agents use; shown in the install command
	TLS                TLS    `yaml:"tls"`
	CASHA256           string `yaml:"ca_sha256"` // the server CA's fingerprint, shown in the install command
	DatabaseDSN        string `yaml:"database_dsn"`
	DatabaseDSNFile    string `yaml:"database_dsn_file"`
	CredentialsKeyFile string `yaml:"credentials_key_file"`
	StateDir           string `yaml:"state_dir"`
	ResticBin          string `yaml:"restic_bin"`
	DefaultZone        string `yaml:"default_zone"` // IANA zone for schedules without one (plan-m1 Q6)
	NodeName           string `yaml:"node_name"`

	Storage     StorageSettings     `yaml:"storage"`
	Maintenance MaintenanceSettings `yaml:"maintenance"`

	Auth map[string]any `yaml:"auth"` // spec §6.4; M4
}

// TLS is the certificate for the agent listener. Without it the listener is plain HTTP, which is
// for tests only and logged as such.
type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// StorageSettings locate the bucket and the identities the server uses.
type StorageSettings struct {
	Endpoint                   string   `yaml:"endpoint"`
	Bucket                     string   `yaml:"bucket"`
	Region                     string   `yaml:"region"`
	MaintenanceCredentialsFile string   `yaml:"maintenance_credentials_file"` // KEY=VALUE, root-only
	ProvisionerCommand         []string `yaml:"provisioner_command"`
}

// MaintenanceSettings control the repository checks and retention.
type MaintenanceSettings struct {
	// Check is "daily" (the default): each agent checks its own repository once a day, right after a
	// backup, reading 1/check_cycle_days of the data (decision 030). "off" leaves only `holdfast check`.
	Check          string `yaml:"check"`
	CheckCycleDays int    `yaml:"check_cycle_days"` // read-data-subset cycle, default 30
	// Retention is "daily" (the default): the server forgets and prunes each host once a day, in its quiet
	// stretch, behind the 7-day protection (plan-m3). "off" forgets nothing by itself; `holdfast retention
	// run`, Prune now and Delete now still work.
	Retention string `yaml:"retention"`
}

// Values of MaintenanceSettings.Check and MaintenanceSettings.Retention.
const (
	CheckDaily = "daily"
	CheckOff   = "off"
)

// LoadSettings reads and validates the settings file. Unknown keys are refused, so a typo is an
// error rather than a silently ignored setting.
func LoadSettings(path string) (Settings, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Settings{}, err
	}
	var s Settings
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return Settings{}, fmt.Errorf("%s: %w", path, err)
	}
	if s.DatabaseDSN == "" && s.DatabaseDSNFile != "" {
		dsn, err := os.ReadFile(s.DatabaseDSNFile)
		if err != nil {
			return Settings{}, err
		}
		s.DatabaseDSN = strings.TrimSpace(string(dsn))
	}
	s.defaults()
	return s, s.validate()
}

func (s *Settings) defaults() {
	if s.Listen == "" {
		s.Listen = ":8443"
	}
	if s.StateDir == "" {
		s.StateDir = "/var/lib/holdfast-server"
	}
	if s.DefaultZone == "" {
		s.DefaultZone = "UTC"
	}
	if s.ResticBin == "" {
		s.ResticBin = restic.DefaultBin // where the server's package puts the pinned restic (decision 032)
	}
	if s.Maintenance.Check == "" {
		s.Maintenance.Check = CheckDaily
	}
	if s.Maintenance.CheckCycleDays <= 0 {
		s.Maintenance.CheckCycleDays = 30
	}
	if s.Maintenance.Retention == "" {
		s.Maintenance.Retention = CheckDaily
	}
	if s.NodeName == "" {
		s.NodeName, _ = os.Hostname()
	}
}

func (s Settings) validate() error {
	var errs []error
	need := func(v, name string) {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	need(s.DatabaseDSN, "database_dsn (or database_dsn_file)")
	need(s.CredentialsKeyFile, "credentials_key_file")
	need(s.PublicURL, "public_url")
	need(s.Storage.Endpoint, "storage.endpoint")
	need(s.Storage.Bucket, "storage.bucket")
	need(s.Storage.MaintenanceCredentialsFile, "storage.maintenance_credentials_file")
	if _, err := time.LoadLocation(s.DefaultZone); err != nil {
		errs = append(errs, fmt.Errorf("default_zone: %w", err))
	}
	if c := s.Maintenance.Check; c != CheckDaily && c != CheckOff {
		errs = append(errs, fmt.Errorf("maintenance.check is %q; it must be %q or %q", c, CheckDaily, CheckOff))
	}
	if c := s.Maintenance.Retention; c != CheckDaily && c != CheckOff {
		errs = append(errs, fmt.Errorf("maintenance.retention is %q; it must be %q or %q", c, CheckDaily, CheckOff))
	}
	return errors.Join(errs...)
}
