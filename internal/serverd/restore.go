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
	"sort"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"

	"github.com/kabsdk/holdfast/internal/recovery"
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
	identity := fs.String("identity", "", "the recovery file, or the escrow's age key file, which the export is encrypted to; read once and never kept")
	latest := fs.Bool("latest", false, "restore the newest export in the bucket, read with the maintenance key")
	fs.Usage = func() {
		fmt.Fprintf(out, "usage: %s restore-export [-config server.yaml] -identity <recovery file> (<export file> | -latest)\n", name)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	file := fs.Arg(0)
	switch {
	case *identity == "":
		return errors.New("restore-export needs -identity, the recovery file (or the escrow's age key file)")
	case fs.NArg() > 1, file == "" && !*latest, file != "" && *latest:
		fs.Usage()
		return errors.New("give the export's file, or -latest")
	}
	set, err := server.LoadSettings(*cfgPath)
	if err != nil {
		return err
	}
	rec, err := recovery.ReadFile(*identity)
	if err != nil {
		return err
	}
	if len(rec.Identities) == 0 {
		return fmt.Errorf("%s holds no age key (AGE-SECRET-KEY-…): add the escrow's age key to it", *identity)
	}
	ids := rec.Identities
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	maint, err := storage.ReadKeyFile(set.Storage.MaintenanceCredentialsFile)
	if err != nil {
		return err
	}
	bucket := storage.S3Bucket{Location: storage.Location{Endpoint: set.Storage.Endpoint, Bucket: set.Storage.Bucket, Region: set.Storage.Region}, Maint: maint}
	var data []byte
	exportName := filepath.Base(file)
	if *latest {
		b := bucket
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

	// The credentials key: the recovery file's, or the one where server.yaml says (plan-m10 item 3). The recovery
	// file's is written there only once the export has been checked with it, below.
	key, err := creds.ReadKeyFile(set.CredentialsKeyFile)
	writeKey := false
	switch {
	case errors.Is(err, os.ErrNotExist) && rec.CredentialsKey != nil:
		key, writeKey = rec.CredentialsKey, true
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%s holds no credentials key (HOLDFAST-CREDENTIALS-KEY-…), and %s does not exist: give the recovery file this server's installer wrote", *identity, set.CredentialsKeyFile)
	case err != nil:
		return fmt.Errorf("the credentials key (credentials_key_file): %w", err)
	case rec.CredentialsKey != nil && !bytes.Equal(key, rec.CredentialsKey):
		return fmt.Errorf("%s holds another credentials key than the recovery file: remove it, or give the matching recovery file", set.CredentialsKeyFile)
	}
	sealer, err := creds.NewSealer(key)
	if err != nil {
		return fmt.Errorf("the credentials key: %w", err)
	}

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
	// Everything is checked: the recovery file's credentials key opened the export's passwords, so it is this server's.
	if writeKey {
		if err := creds.WriteKeyFile(set.CredentialsKeyFile, key); err != nil {
			return fmt.Errorf("writing the credentials key: %w", err)
		}
		fmt.Fprintf(out, "wrote the credentials key from the recovery file to %s\n", set.CredentialsKeyFile)
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

	restoreFiles(out, set, *cfgPath, c.Files)
	back := hostsFromEscrow(ctx, out, set, bucket, stores["public"], sealer, maint, ids, hosts)
	fmt.Fprintln(out, "now start holdfast-server: agents enrolled before the export reconnect by themselves (docs/runbooks/rebuild-holdfast.md)")
	for _, fqdn := range back {
		fmt.Fprintf(out, "  %s came back from escrow/: enroll it again (holdfast host enroll-code %s, and the line it prints), and set its jobs\n", fqdn, fqdn)
	}
	return nil
}

// restoreFiles writes the server's files the export holds (plan-m10 item 3) where server.yaml says, and never over a
// file that is there. The exported settings go beside server.yaml, to compare.
func restoreFiles(out io.Writer, set server.Settings, cfgPath string, files map[string][]byte) {
	paths := server.ServerFiles(set)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		p, mode := paths[name], os.FileMode(0o644)
		switch {
		case name == "server.yaml":
			p, mode = cfgPath+".from-export", 0o600
		case strings.HasSuffix(name, "key_file"):
			mode = 0o600
		}
		if p == "" {
			fmt.Fprintf(out, "the export holds %s, but server.yaml names no path for it: not written\n", name)
			continue
		}
		if _, err := os.Stat(p); err == nil {
			fmt.Fprintf(out, "kept %s, which is there already (the export's %s is not written)\n", p, name)
			continue
		}
		err := os.MkdirAll(filepath.Dir(p), 0o700)
		if err == nil {
			err = os.WriteFile(p, files[name], mode)
		}
		if err != nil {
			fmt.Fprintf(out, "could not write %s: %v\n", p, err)
			continue
		}
		fmt.Fprintf(out, "wrote %s from the export (%s)\n", p, name)
	}
}

// hostsFromEscrow brings back each host that has an escrow file in the bucket but is not in the export: one added after
// it (plan-m10 item 5). It is what holdfast host import does, for each: the escrow file decrypted with the recovery
// file's age key, checked, and the host added with its repository and password, and no job.
func hostsFromEscrow(ctx context.Context, out io.Writer, set server.Settings, bucket storage.S3Bucket, st *store.Store, sealer *creds.Sealer,
	maint storage.Key, ids []age.Identity, known []store.Host) []string {
	names, err := bucket.ListEscrow(ctx)
	if err != nil {
		fmt.Fprintf(out, "could not list escrow/ for hosts added after the export: %v; holdfast host import brings each back\n", err)
		return nil
	}
	have := map[string]bool{}
	for _, h := range known {
		have[h.FQDN] = true
	}
	var missing []string
	for _, n := range names {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	gw, err := storage.ReadKeyFile(set.Storage.GatewayCredentialsFile)
	if err != nil {
		fmt.Fprintf(out, "the gateway key: %v; hosts added after the export are not brought back\n", err)
		return nil
	}
	srv, err := server.New(set, server.Deps{Store: st, Sealer: sealer, Maintenance: maint, Gateway: gw})
	if err != nil {
		fmt.Fprintf(out, "hosts added after the export are not brought back: %v\n", err)
		return nil
	}
	// Root at the command line; the revision's message says it was restore-export.
	root := server.Actor{Name: "local:root", Source: "cli", Everywhere: true}
	var back []string
	for _, fqdn := range missing {
		enc, err := bucket.GetEscrow(ctx, fqdn)
		var plain []byte
		if err == nil {
			var r io.Reader
			if r, err = age.Decrypt(bytes.NewReader(enc), ids...); err == nil {
				plain, err = io.ReadAll(r)
			}
		}
		if err == nil {
			_, err = srv.ImportHost(ctx, root, server.ImportHostInput{FQDN: fqdn, EscrowFile: string(plain),
				Message: "Brought back from escrow/ by restore-export: added after the export (plan-m10)"})
		}
		if err != nil {
			fmt.Fprintf(out, "%s has an escrow file but could not be brought back: %v; holdfast host import, once the server runs\n", fqdn, err)
			continue
		}
		back = append(back, fqdn)
	}
	return back
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
