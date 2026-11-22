package server

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"filippo.io/age"
	"go.yaml.in/yaml/v3"

	"github.com/kabsdk/holdfast/internal/restic"
)

// Settings is /etc/holdfast/server.yaml. Changing it needs root on the server (spec §3.3). ui and auth are
// spec §6.4 (decision 034); the rest is operational (plan-m1 §9).
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
	Escrow      EscrowSettings      `yaml:"escrow"`

	UI   UISettings   `yaml:"ui"`
	Auth AuthSettings `yaml:"auth"`

	// Role is "server" (the default) or "vault": holdfast-vault, which copies another site's hosts into this
	// site's bucket (decisions 027 and 039). Source is then that site's holdfast-server.
	Role   string         `yaml:"role"`
	Source SourceSettings `yaml:"source"`
}

// Roles of a holdfast-server process.
const (
	RoleServer = "server"
	RoleVault  = "vault"
)

// SourceSettings are, for a vault, the holdfast-server it copies from.
type SourceSettings struct {
	Server    string        `yaml:"server"`     // its agent listener, such as https://holdfast.site-a.example:8443
	CASHA256  string        `yaml:"ca_sha256"`  // its CA's fingerprint, as agents pin it
	TokenFile string        `yaml:"token_file"` // the token `holdfast vault add` printed, alone in a root-only file
	Poll      time.Duration `yaml:"poll"`       // how often it syncs and copies; default 5m
	RcloneBin string        `yaml:"rclone_bin"` // default /usr/libexec/holdfast/rclone, which the vault's package ships
}

// UISettings are the listener for people: the UI and /api/v1 over HTTPS, with sessions (decision 034). It is
// off while Listen is empty.
type UISettings struct {
	Listen    string `yaml:"listen"`     // such as ":443"
	PublicURL string `yaml:"public_url"` // the URL people use; the OIDC redirect goes back to it
	TLS       TLS    `yaml:"tls"`        // a certificate browsers trust; without it plain HTTP, for tests only
}

// AuthSettings are the OIDC provider and the bootstrap admins (spec §6.4).
type AuthSettings struct {
	OIDC OIDCSettings `yaml:"oidc"`
	// BootstrapAdmins are usernames that are admin on every host whatever their groups, for the first
	// login. Each login that relies on it is audited.
	BootstrapAdmins []string `yaml:"bootstrap_admins"`
}

// OIDCSettings is the provider. The server is its only client (decision 034).
type OIDCSettings struct {
	Issuer           string   `yaml:"issuer"`
	ClientID         string   `yaml:"client_id"`
	ClientSecretFile string   `yaml:"client_secret_file"` // the client secret, alone in a root-only file
	Scopes           []string `yaml:"scopes"`             // default: openid, profile, groups
	GroupsClaim      string   `yaml:"groups_claim"`       // default: groups
	UsernameClaim    string   `yaml:"username_claim"`     // default: preferred_username
	CAFile           string   `yaml:"ca_file"`            // a CA to trust for the issuer, if not the system's
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

// EscrowSettings name who can read the escrow files the server writes to the bucket (decision 036).
type EscrowSettings struct {
	// AgeRecipient is an age public key, age1…, whose private key the admin keeps offline. Empty: no escrow
	// file is written, and every host's page warns.
	AgeRecipient string `yaml:"age_recipient"`
}

// IsVault says whether this process is holdfast-vault.
func (s Settings) IsVault() bool { return s.Role == RoleVault }

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
	if s.Role == "" {
		s.Role = RoleServer
	}
	if s.Source.Poll <= 0 {
		s.Source.Poll = 5 * time.Minute
	}
	if s.Source.RcloneBin == "" {
		s.Source.RcloneBin = "/usr/libexec/holdfast/rclone"
	}
	o := &s.Auth.OIDC
	if len(o.Scopes) == 0 {
		o.Scopes = []string{"openid", "profile", "groups"}
	}
	if o.GroupsClaim == "" {
		o.GroupsClaim = "groups"
	}
	if o.UsernameClaim == "" {
		o.UsernameClaim = "preferred_username"
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
	switch s.Role {
	case RoleServer:
	case RoleVault:
		need(s.Source.Server, "source.server (role is vault)")
		need(s.Source.TokenFile, "source.token_file (role is vault)")
	default:
		errs = append(errs, fmt.Errorf("role is %q; it must be %q or %q", s.Role, RoleServer, RoleVault))
	}
	if r := s.Escrow.AgeRecipient; r != "" {
		if _, err := age.ParseX25519Recipient(r); err != nil {
			errs = append(errs, fmt.Errorf("escrow.age_recipient: %w (it is an age public key, age1…, made with age-keygen)", err))
		}
	}
	if s.UI.Listen != "" {
		need(s.UI.PublicURL, "ui.public_url (ui.listen is set)")
		need(s.Auth.OIDC.Issuer, "auth.oidc.issuer (ui.listen is set)")
		need(s.Auth.OIDC.ClientID, "auth.oidc.client_id (ui.listen is set)")
		need(s.Auth.OIDC.ClientSecretFile, "auth.oidc.client_secret_file (ui.listen is set)")
	}
	return errors.Join(errs...)
}
