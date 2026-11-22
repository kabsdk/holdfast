package serverd

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"

	"github.com/kabsdk/holdfast/internal/server"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/export"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// restoreExport is `holdfast-server restore-export`: it loads a nightly export into a new, empty database, on a
// server rebuilt from its packages, server.yaml, the key files and the credentials key (plan-m9 §2.3, Q5). It runs
// while holdfast-server is stopped, as root on the server: it is not an API, as there is no server yet.
func restoreExport(name, defaultConfig string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet(name+" restore-export", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", defaultConfig, "settings file")
	identity := fs.String("identity", "", "the escrow's age key file, which the export is encrypted to; read once and never kept")
	latest := fs.Bool("latest", false, "restore the newest export in the bucket, read with the maintenance key")
	fs.Usage = func() {
		fmt.Fprintf(out, "usage: %s restore-export [-config server.yaml] -identity <age key file> (<export file> | -latest)\n", name)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	file := fs.Arg(0)
	switch {
	case *identity == "":
		return errors.New("restore-export needs -identity, the escrow's age key file")
	case fs.NArg() > 1, file == "" && !*latest, file != "" && *latest:
		fs.Usage()
		return errors.New("give the export's file, or -latest")
	}
	set, err := server.LoadSettings(*cfgPath)
	if err != nil {
		return err
	}
	sealer, err := creds.LoadKeyFile(set.CredentialsKeyFile)
	if err != nil {
		return fmt.Errorf("the credentials key (credentials_key_file): %w", err)
	}
	ids, err := readIdentities(*identity)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var data []byte
	exportName := filepath.Base(file)
	if *latest {
		maint, err := storage.ReadKeyFile(set.Storage.MaintenanceCredentialsFile)
		if err != nil {
			return err
		}
		b := storage.S3Bucket{Location: storage.Location{Endpoint: set.Storage.Endpoint, Bucket: set.Storage.Bucket, Region: set.Storage.Region}, Maint: maint}
		names, err := b.ListMeta(ctx)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return fmt.Errorf("bucket %s holds no export under meta/", set.Storage.Bucket)
		}
		exportName = names[len(names)-1]
		if data, err = b.GetMeta(ctx, exportName); err != nil {
			return err
		}
		fmt.Fprintf(out, "read %s from bucket %s\n", storage.MetaKey(exportName), set.Storage.Bucket)
	} else if data, err = os.ReadFile(file); err != nil {
		return err
	}
	c, err := export.Open(data, ids...)
	if err != nil {
		return err
	}
	m := c.Manifest
	fmt.Fprintf(out, "export %s: written %s by Holdfast %s on %s\n", exportName, m.CreatedAt.UTC().Format(time.RFC3339), m.HoldfastVersion, m.Node)

	// Everything is checked before the database is touched.
	latestVersion := store.LatestVersion()
	stores := map[string]*store.Store{}
	defer func() {
		for _, st := range stores {
			st.Close()
		}
	}()
	for _, sc := range m.Schemas {
		switch {
		case sc.Name != "public" && sc.Name != server.OffsiteSchema:
			return fmt.Errorf("the export has schema %q, which Holdfast does not make", sc.Name)
		case sc.Version > latestVersion:
			return fmt.Errorf("the export's %s tables are at schema version %d, newer than this Holdfast's %d: install Holdfast %s or later, then restore",
				sc.Name, sc.Version, latestVersion, m.HoldfastVersion)
		case sc.Version < 1:
			return fmt.Errorf("the export's %s tables have no schema version", sc.Name)
		}
		if err := checkCredentialsKey(c.Tables[sc.Name], sealer); err != nil {
			return fmt.Errorf("the %s tables: %w", sc.Name, err)
		}
	}
	for _, sc := range m.Schemas {
		var st *store.Store
		if sc.Name == "public" {
			st, err = store.Open(ctx, set.DatabaseDSN)
		} else {
			st, err = store.OpenSchema(ctx, set.DatabaseDSN, sc.Name)
		}
		if err != nil {
			return fmt.Errorf("database: %w", err)
		}
		stores[sc.Name] = st
		empty, err := st.Empty(ctx)
		if err != nil {
			return fmt.Errorf("database: %w", err)
		}
		if !empty {
			return fmt.Errorf("the database already holds Holdfast's %s tables: restore-export loads an export only into a new, empty database (docs/runbooks/rebuild-holdfast.md)", sc.Name)
		}
	}
	switch _, hasOffsite := stores[server.OffsiteSchema]; {
	case set.Offsite != nil && !hasOffsite:
		fmt.Fprintln(out, "note: server.yaml has offsite:, but the export has no off-site tables: the off-site copies start over")
	case set.Offsite == nil && hasOffsite:
		fmt.Fprintln(out, "note: the export has off-site tables, but server.yaml has no offsite:; they are restored all the same")
	}

	for _, sc := range m.Schemas {
		st := stores[sc.Name]
		if err := st.MigrateTo(ctx, sc.Version); err != nil {
			return fmt.Errorf("migrating the %s tables to the export's version %d: %w", sc.Name, sc.Version, err)
		}
		if err := st.LoadTables(ctx, c.Tables[sc.Name]); err != nil {
			return fmt.Errorf("loading the %s tables: %w", sc.Name, err)
		}
		if err := st.Migrate(ctx); err != nil {
			return fmt.Errorf("migrating the %s tables on to version %d: %w", sc.Name, latestVersion, err)
		}
		rows := int64(0)
		for _, t := range c.Tables[sc.Name] {
			rows += t.Rows
		}
		fmt.Fprintf(out, "loaded the %s tables: %d tables, %d rows, from schema version %d, now %d\n", sc.Name, len(c.Tables[sc.Name]), rows, sc.Version, latestVersion)
	}

	q := stores["public"].Q()
	head := int64(0)
	if h, err := q.Head(ctx); err == nil {
		head = h.Number
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	now := time.Now().UTC()
	if err := q.RecordRestoreFromExport(ctx, store.RestoreFromExport{RestoredAt: now, ExportName: exportName, ExportedAt: m.CreatedAt, Head: head}); err != nil {
		return err
	}
	schemas := []map[string]any{}
	for _, sc := range m.Schemas {
		schemas = append(schemas, map[string]any{"name": sc.Name, "version": sc.Version, "tables": len(c.Tables[sc.Name])})
	}
	if err := q.Audit(ctx, store.AuditEntry{Actor: "root", Source: "holdfast-server restore-export", Action: "server.restored", Revision: head,
		Detail: map[string]any{"export": exportName, "exported_at": m.CreatedAt, "exported_by": m.HoldfastVersion, "schemas": schemas}}); err != nil {
		return err
	}
	hosts, _ := q.ListHosts(ctx)
	fmt.Fprintf(out, "restored: head revision %d, %d hosts. What changed after %s is not here.\n", head, len(hosts), m.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Fprintln(out, "now start holdfast-server: agents enrolled before the export reconnect by themselves (docs/runbooks/rebuild-holdfast.md)")
	return nil
}

// readIdentities reads the age key file once.
func readIdentities(path string) ([]age.Identity, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ids, err := age.ParseIdentities(f)
	if err != nil {
		return nil, fmt.Errorf("%s is not an age key file: %w", path, err)
	}
	return ids, nil
}

// checkCredentialsKey opens one sealed repository password of the export with the credentials key, so a wrong key is
// refused before anything is loaded: without the right one, no restored host's repository opens.
func checkCredentialsKey(tables []store.TableData, sealer *creds.Sealer) error {
	for _, t := range tables {
		if t.Name != "host_credentials" || t.Rows == 0 {
			continue
		}
		r := csv.NewReader(bytes.NewReader(t.CSV))
		header, err := r.Read()
		if err != nil {
			return err
		}
		col := map[string]int{}
		for i, h := range header {
			col[h] = i
		}
		fi, ok1 := col["fqdn"]
		pi, ok2 := col["repo_password"]
		if !ok1 || !ok2 {
			return errors.New("host_credentials has no fqdn or repo_password column")
		}
		row, err := r.Read()
		if err != nil {
			return err
		}
		sealed, err := hex.DecodeString(strings.TrimPrefix(row[pi], `\x`))
		if err != nil {
			return fmt.Errorf("host_credentials: %w", err)
		}
		if _, err := sealer.Open(sealed, row[fi]); err != nil {
			return fmt.Errorf("the credentials key does not open the export's repository passwords: is credentials_key_file this server's key, from escrow? (%v)", err)
		}
		return nil
	}
	return nil
}
