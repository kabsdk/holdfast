package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ExportRecord is one attempt at the nightly export (decision 040).
type ExportRecord struct {
	ID           int64     `json:"id"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	Actor        string    `json:"actor"`
	Name         string    `json:"name"` // meta/<name> in the bucket
	Bytes        int64     `json:"bytes"`
	Error        string    `json:"error,omitempty"`
	Offsite      bool      `json:"offsite"` // it was to be written to the off-site bucket too
	OffsiteError string    `json:"offsite_error,omitempty"`
}

const exportCols = `id, started_at, finished_at, actor, name, bytes, error, offsite, offsite_error`

func scanExport(row pgx.Row) (ExportRecord, error) {
	var e ExportRecord
	err := row.Scan(&e.ID, &e.StartedAt, &e.FinishedAt, &e.Actor, &e.Name, &e.Bytes, &e.Error, &e.Offsite, &e.OffsiteError)
	return e, err
}

// RecordExport keeps one attempt, and returns its id.
func (q *Q) RecordExport(ctx context.Context, e ExportRecord) (int64, error) {
	var id int64
	err := q.db.QueryRow(ctx, `INSERT INTO exports (started_at, finished_at, actor, name, bytes, error, offsite, offsite_error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		e.StartedAt, e.FinishedAt, e.Actor, e.Name, e.Bytes, e.Error, e.Offsite, e.OffsiteError).Scan(&id)
	return id, err
}

// ListExports returns the newest attempts first.
func (q *Q) ListExports(ctx context.Context, limit int) ([]ExportRecord, error) {
	rows, err := q.db.Query(ctx, `SELECT `+exportCols+` FROM exports ORDER BY started_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExportRecord{}
	for rows.Next() {
		e, err := scanExport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ExportState is what the server needs to know about its exports: the newest attempt, the newest one written, and
// the newest one written off-site. Each is nil when there is none.
type ExportState struct {
	Last        *ExportRecord `json:"last,omitempty"`
	LastGood    *ExportRecord `json:"last_good,omitempty"`
	LastOffsite *ExportRecord `json:"last_offsite,omitempty"`
}

// ExportState reads it.
func (q *Q) ExportState(ctx context.Context) (ExportState, error) {
	var st ExportState
	one := func(where string) (*ExportRecord, error) {
		e, err := scanExport(q.db.QueryRow(ctx, `SELECT `+exportCols+` FROM exports `+where+` ORDER BY started_at DESC, id DESC LIMIT 1`))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &e, nil
	}
	var err error
	if st.Last, err = one(""); err != nil {
		return st, err
	}
	if st.LastGood, err = one(`WHERE error = ''`); err != nil {
		return st, err
	}
	st.LastOffsite, err = one(`WHERE error = '' AND offsite AND offsite_error = ''`)
	return st, err
}

// RestoreFromExport is a server restored from an export.
type RestoreFromExport struct {
	RestoredAt time.Time `json:"restored_at"`
	ExportName string    `json:"export_name"`
	ExportedAt time.Time `json:"exported_at"`
	Head       int64     `json:"head"` // the head revision it was restored at
}

// RecordRestoreFromExport keeps that the database was restored from an export.
func (q *Q) RecordRestoreFromExport(ctx context.Context, r RestoreFromExport) error {
	_, err := q.db.Exec(ctx, `INSERT INTO restores_from_export (restored_at, export_name, exported_at, head) VALUES ($1, $2, $3, $4)`,
		r.RestoredAt, r.ExportName, r.ExportedAt, r.Head)
	return err
}

// LastRestoreFromExport returns the latest restore, or nil when the database was never restored.
func (q *Q) LastRestoreFromExport(ctx context.Context) (*RestoreFromExport, error) {
	var r RestoreFromExport
	err := q.db.QueryRow(ctx, `SELECT restored_at, export_name, exported_at, head FROM restores_from_export ORDER BY id DESC LIMIT 1`).
		Scan(&r.RestoredAt, &r.ExportName, &r.ExportedAt, &r.Head)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}
