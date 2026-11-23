package store

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The nightly export's tables (decision 040): each one as CSV with a header, read with COPY in one
// consistent snapshot, and loaded back with COPY into a new, empty database. Plain CSV, so restoring needs no pg_dump
// of a matching version, and the files can be read without Holdfast.

// exportSkip are left out of an export: the sessions, stale after a rebuild, whose users log in again;
// the gateway's grants, each of which lasts one restore, which a rebuild ends; the alerts sent,
// which a rebuilt server sends again for what is still wrong; and the migrations' own table, which restoring makes
// again.
var exportSkip = map[string]bool{"sessions": true, "gateway_grants": true, "notify_state": true, "schema_migrations": true}

// identifier is a table or column name Holdfast's migrations make.
var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// TableData is one table of an export: its CSV, header first, and how many rows it holds.
type TableData struct {
	Name string
	CSV  []byte
	Rows int64
}

// LatestVersion is the newest migration this build has.
func LatestVersion() int {
	names, _ := fs.Glob(migrationFS, "migrations/*.sql")
	v := 0
	for _, n := range names {
		if x, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(n, "migrations/"), "_", 2)[0]); err == nil && x > v {
			v = x
		}
	}
	return v
}

// SchemaVersion is the newest migration applied to the store's schema; 0 when none is.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name = 'schema_migrations')`).Scan(&exists); err != nil || !exists {
		return 0, err
	}
	var v int
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

// Empty says whether the store's schema holds no table: restoring an export refuses anything else.
func (s *Store) Empty(ctx context.Context) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema()`).Scan(&n)
	return n == 0, err
}

// ExportTables reads every table but exportSkip's, in one read-only snapshot, so the tables agree with each other.
// It returns them by name, and the schema version they are at.
func (s *Store) ExportTables(ctx context.Context) ([]TableData, int, error) {
	var out []TableData
	version := 0
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
			return err
		}
		names, err := tableNames(ctx, tx)
		if err != nil {
			return err
		}
		for _, name := range names {
			if exportSkip[name] {
				continue
			}
			var buf bytes.Buffer
			tag, err := tx.Conn().PgConn().CopyTo(ctx, &buf, `COPY `+name+` TO STDOUT WITH (FORMAT csv, HEADER true)`)
			if err != nil {
				return fmt.Errorf("reading table %s: %w", name, err)
			}
			out = append(out, TableData{Name: name, CSV: buf.Bytes(), Rows: tag.RowsAffected()})
		}
		return nil
	})
	return out, version, err
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// tableNames lists the base tables of the current schema, by name.
func tableNames(ctx context.Context, q querier) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT table_name::text FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if !identifier.MatchString(n) {
			return nil, fmt.Errorf("table %q is not a name Holdfast makes", n)
		}
	}
	return names, nil
}

// LoadTables loads an export's tables into the store's schema, which must be migrated to the export's version and
// hold no rows yet. It loads in one transaction, each table after those it refers to, with the columns its CSV header
// names, then moves each serial column's sequence past the largest value loaded.
func (s *Store) LoadTables(ctx context.Context, tables []TableData) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		names, err := tableNames(ctx, tx)
		if err != nil {
			return err
		}
		known := map[string]bool{}
		for _, n := range names {
			known[n] = true
		}
		byName := map[string]TableData{}
		for _, t := range tables {
			if !known[t.Name] || exportSkip[t.Name] {
				return fmt.Errorf("the export has a table %q that this schema version does not", t.Name)
			}
			if _, dup := byName[t.Name]; dup {
				return fmt.Errorf("the export has table %q twice", t.Name)
			}
			byName[t.Name] = t
		}
		order, err := loadOrder(ctx, tx, names)
		if err != nil {
			return err
		}
		for _, name := range order {
			t, ok := byName[name]
			if !ok {
				continue
			}
			cols, err := csvHeader(t.CSV)
			if err != nil {
				return fmt.Errorf("table %s: %w", name, err)
			}
			tag, err := tx.Conn().PgConn().CopyFrom(ctx, bytes.NewReader(t.CSV),
				`COPY `+name+` (`+strings.Join(cols, ", ")+`) FROM STDIN WITH (FORMAT csv, HEADER true)`)
			if err != nil {
				return fmt.Errorf("loading table %s: %w", name, err)
			}
			if tag.RowsAffected() != t.Rows {
				return fmt.Errorf("table %s: loaded %d rows, the export says %d", name, tag.RowsAffected(), t.Rows)
			}
		}
		return resetSequences(ctx, tx)
	})
}

// csvHeader returns the column names a table's CSV starts with.
func csvHeader(data []byte) ([]string, error) {
	cols, err := csv.NewReader(bytes.NewReader(data)).Read()
	if err != nil {
		return nil, fmt.Errorf("its CSV has no header: %w", err)
	}
	for _, c := range cols {
		if !identifier.MatchString(c) {
			return nil, fmt.Errorf("column %q is not a name Holdfast makes", c)
		}
	}
	return cols, nil
}

// loadOrder sorts the tables so that each comes after every table its foreign keys refer to.
func loadOrder(ctx context.Context, tx pgx.Tx, names []string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT child.relname::text, parent.relname::text FROM pg_constraint c
		JOIN pg_class child ON child.oid = c.conrelid JOIN pg_class parent ON parent.oid = c.confrelid
		JOIN pg_namespace n ON n.oid = child.relnamespace
		WHERE c.contype = 'f' AND n.nspname = current_schema()`)
	if err != nil {
		return nil, err
	}
	parents := map[string][]string{}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			rows.Close()
			return nil, err
		}
		if child != parent {
			parents[child] = append(parents[child], parent)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var order []string
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(string) error
	visit = func(n string) error {
		switch state[n] {
		case 1:
			return errors.New("the tables' foreign keys form a cycle at " + n)
		case 2:
			return nil
		}
		state[n] = 1
		ps := parents[n]
		sort.Strings(ps)
		for _, p := range ps {
			if err := visit(p); err != nil {
				return err
			}
		}
		state[n] = 2
		order = append(order, n)
		return nil
	}
	for _, n := range names {
		if err := visit(n); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// resetSequences moves every serial column's sequence past the largest value in its column.
func resetSequences(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT table_name::text, column_name::text, pg_get_serial_sequence(table_name::text, column_name::text)
		FROM information_schema.columns WHERE table_schema = current_schema() AND column_default LIKE 'nextval(%'`)
	if err != nil {
		return err
	}
	type serial struct{ table, column, seq string }
	var all []serial
	for rows.Next() {
		var s serial
		var seq *string
		if err := rows.Scan(&s.table, &s.column, &seq); err != nil {
			rows.Close()
			return err
		}
		if seq != nil && identifier.MatchString(s.table) && identifier.MatchString(s.column) {
			s.seq = *seq
			all = append(all, s)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, s := range all {
		if _, err := tx.Exec(ctx, `SELECT setval($1::regclass, COALESCE((SELECT MAX(`+s.column+`) FROM `+s.table+`), 0) + 1, false)`, s.seq); err != nil {
			return fmt.Errorf("the sequence of %s.%s: %w", s.table, s.column, err)
		}
	}
	return nil
}
