package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// EnginePostgreSQL backs up one PostgreSQL database, or a server's roles and other globals, with pg_dump or
// pg_dumpall streamed into restic (decision 055). restic still stores, encrypts and keeps every byte.
const EnginePostgreSQL = "postgresql"

// PGConnection is how an agent reaches a PostgreSQL server: its address, and who it signs in as. It is set
// centrally, in the UI, the API or the CLI, and is part of every revision. Its passwords are not: they are kept
// sealed, beside the repository passwords, and sent only to the agent (decision 055).
type PGConnection struct {
	// URL is libpq's connection URI without a user, password or database, such as
	// postgresql://db.example.internal:5432/?sslmode=verify-full&sslrootcert=/etc/holdfast/pg/ca.crt, or
	// postgresql:///?host=/var/run/postgresql for a local socket. Paths in it are on the agent.
	URL string `json:"url"`
	// User signs in to back up: it reads every database (pg_read_all_data).
	User string `json:"user"`
	// RestoreUser, when set, restores: it may create databases and act as their owners. Without it, a dump of this
	// connection restores as a file only.
	RestoreUser string `json:"restore_user,omitempty"`
}

// PGJob is what a PostgreSQL job backs up: one database of a connection, or the connection's roles and other
// globals (pg_dumpall --globals-only).
type PGJob struct {
	Connection string `json:"connection"`
	Database   string `json:"database,omitempty"`
	Globals    bool   `json:"globals,omitempty"`
}

// IsPostgreSQL says whether the job backs up a PostgreSQL database or its globals.
func (j Job) IsPostgreSQL() bool { return j.Engine == EnginePostgreSQL }

var (
	// connNameRE is a connection's name, as a job ID is spelled.
	connNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// pgUserRE is a role name Holdfast signs in as: plain, so it never needs quoting on a command line.
	pgUserRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$.-]{0,62}$`)
)

// ValidConnectionName checks a connection's name.
func ValidConnectionName(s string) error {
	if !connNameRE.MatchString(s) {
		return fmt.Errorf("connection name %q is not valid: use lower-case letters, digits and hyphens, at most 63 characters", s)
	}
	return nil
}

// ValidDatabaseName checks a database's name as a job gives it. PostgreSQL allows almost anything; Holdfast takes
// what fits in a file name and needs no quoting on the agent: letters, digits, _, - and ., at most 63 bytes.
func ValidDatabaseName(s string) error {
	if s == "" || len(s) > 63 || strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-.") != "" || s == "." || s == ".." {
		return fmt.Errorf("database name %q is not one Holdfast backs up: use letters, digits, _, - and ., at most 63", s)
	}
	return nil
}

// Validate checks a connection: a postgresql:// URI with no user, password or database in it, and the users.
func (c PGConnection) Validate() []string {
	var out []string
	u, err := url.Parse(c.URL)
	switch {
	case err != nil:
		out = append(out, fmt.Sprintf("the URL is not a URI: %v", err))
	case u.Scheme != "postgresql" && u.Scheme != "postgres":
		out = append(out, "the URL starts with postgresql://")
	case u.User != nil:
		out = append(out, "the URL holds no user or password: they have fields of their own, so the password is never stored in plain text")
	case u.Query().Has("password") || u.Query().Has("user") || u.Query().Has("dbname") || u.Query().Has("passfile"):
		out = append(out, "the URL holds no user, password, passfile or dbname: the user and password have fields of their own, and each job names its database")
	case strings.Trim(u.Path, "/") != "":
		out = append(out, "the URL names no database: each job names its own")
	case u.Host == "" && !u.Query().Has("host"):
		out = append(out, "the URL names a host, such as postgresql://db.example.internal:5432/, or a socket directory, such as postgresql:///?host=/var/run/postgresql")
	}
	if strings.ContainsAny(c.URL, "\x00\n\r") {
		out = append(out, "the URL holds a control character")
	}
	if !pgUserRE.MatchString(c.User) {
		out = append(out, fmt.Sprintf("user %q is not a plain role name", c.User))
	}
	if c.RestoreUser != "" && !pgUserRE.MatchString(c.RestoreUser) {
		out = append(out, fmt.Sprintf("restore user %q is not a plain role name", c.RestoreUser))
	}
	return out
}

// pgTarget is what a PostgreSQL job backs up, so two jobs of one agent never back up the same.
func (p PGJob) target() string {
	if p.Globals {
		return p.Connection + " globals"
	}
	return p.Connection + " " + p.Database
}
