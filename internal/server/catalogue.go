package server

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/restic"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// The snapshot catalogue and the reads. Reads run on the server with the maintenance key and
// --no-lock, so they work while a host is offline and never contend with its agent. Only names
// and metadata come out of them, never file contents.

const (
	catalogueEach = 24 * time.Hour   // a host's catalogue is read at least this often
	readTimeout   = 2 * time.Minute  // one directory listing or diff
	findTimeout   = 5 * time.Minute  // a find over every snapshot
	pageSize      = 1000             // entries per page
	refreshLimit  = 10 * time.Minute // one catalogue refresh
)

// catalogueRefresher coalesces refreshes: one runs per host at a time, and a request that arrives while one
// runs makes it run once more afterwards.
type catalogueRefresher struct {
	mu      sync.Mutex
	running map[string]bool
	again   map[string]bool
}

var snapshotIDPattern = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

func toCatalogueRow(fqdn string, sn restic.Snapshot) store.Snapshot {
	row := store.Snapshot{FQDN: fqdn, ID: sn.ID, ShortID: sn.ShortID, Time: sn.Time, Hostname: sn.Hostname, Tags: sn.Tags, Paths: sn.Paths, Parent: sn.Parent,
		Original: sn.Original}
	for _, t := range sn.Tags {
		if id, ok := strings.CutPrefix(t, "job="); ok {
			row.JobID = id
		}
	}
	if sn.Summary != nil {
		files, bytes, added := sn.Summary.TotalFilesProcessed, sn.Summary.TotalBytesProcessed, sn.Summary.DataAdded
		row.TotalFiles, row.TotalBytes, row.DataAdded = &files, &bytes, &added
	}
	return row
}

// readRunner is a restic runner for one host's repository that never takes a lock.
func (s *Server) readRunner(ctx context.Context, fqdn string) (restic.Runner, func(), error) {
	r, cleanup, err := s.maintRunner(ctx, fqdn)
	if err != nil {
		return restic.Runner{}, nil, err
	}
	r.NoLock = true
	return r, cleanup, nil
}

// RefreshCatalogue reads a host's repository and makes the catalogue match it. Snapshots that are no longer
// there keep a tombstone. A disappearance Holdfast did not cause is audited, and it raises the host's
// banner (decision 032).
func (s *Server) RefreshCatalogue(ctx context.Context, fqdn string) error {
	if _, err := s.st.Q().GetHost(ctx, fqdn); err != nil {
		return fmt.Errorf("host %s: %w", fqdn, err)
	}
	ctx, cancel := context.WithTimeout(ctx, refreshLimit)
	defer cancel()
	r, cleanup, err := s.readRunner(ctx, fqdn)
	if err != nil {
		return err
	}
	defer cleanup()
	// The read counts from when the listing started: a backup reported while restic lists is not in this read, and the
	// storage witness (store.ReportedInBucket) must not take it for missing.
	at := s.now()
	list, err := r.Snapshots(ctx)
	if err != nil {
		_ = s.st.Q().RecordCatalogueError(ctx, fqdn, err.Error())
		return fmt.Errorf("reading the snapshots of %s: %w", fqdn, err)
	}
	rows := make([]store.Snapshot, 0, len(list))
	for _, sn := range list {
		rows = append(rows, toCatalogueRow(fqdn, sn))
	}
	var added, gone int
	if err := s.st.InTx(ctx, func(q *store.Q) error {
		var err error
		added, gone, err = q.SyncSnapshots(ctx, fqdn, rows, at)
		if err != nil || gone == 0 {
			return err
		}
		return q.Audit(ctx, store.AuditEntry{Actor: systemActor.Name, Source: systemActor.Source, Action: "snapshot.disappeared", FQDN: fqdn,
			Detail: map[string]any{"count": gone}})
	}); err != nil {
		return err
	}
	if gone > 0 {
		s.log.Warn("snapshots disappeared from a repository without Holdfast deleting them", "host", fqdn, "count", gone)
	}
	// The repository's passwords: one added outside Holdfast raises a banner (decision 032).
	if keys, err := r.Keys(ctx); err != nil {
		s.log.Warn("listing the repository's keys", "host", fqdn, "err", err)
	} else {
		ids := make([]string, 0, len(keys))
		for _, k := range keys {
			ids = append(ids, k.ID)
		}
		err := s.st.InTx(ctx, func(q *store.Q) error {
			_, fresh, err := q.RecordRepositoryKeys(ctx, fqdn, ids)
			if err != nil || len(fresh) == 0 {
				return err
			}
			s.log.Warn("a restic password was added to a repository outside Holdfast", "host", fqdn, "keys", fresh)
			return q.Audit(ctx, store.AuditEntry{Actor: systemActor.Name, Source: systemActor.Source, Action: "repository.key_added_outside", FQDN: fqdn,
				Detail: map[string]any{"keys": fresh}})
		})
		if err != nil {
			return err
		}
	}
	if added > 0 {
		s.log.Debug("catalogue refreshed", "host", fqdn, "added", added)
	}
	return nil
}

// refreshSoon refreshes a host's catalogue in the background, after a backup event.
func (s *Server) refreshSoon(fqdn string) {
	c := &s.refresher
	c.mu.Lock()
	if c.running[fqdn] {
		c.again[fqdn] = true
		c.mu.Unlock()
		return
	}
	c.running[fqdn] = true
	c.mu.Unlock()
	go func() {
		for {
			if err := s.RefreshCatalogue(s.background(), fqdn); err != nil {
				s.log.Warn("refreshing the snapshot catalogue", "host", fqdn, "err", err)
			}
			c.mu.Lock()
			if !c.again[fqdn] {
				delete(c.running, fqdn)
				c.mu.Unlock()
				return
			}
			delete(c.again, fqdn)
			c.mu.Unlock()
		}
	}()
}

// refreshCatalogueDue refreshes the catalogue of each enrolled host not read for a day.
func (s *Server) refreshCatalogueDue(ctx context.Context) {
	hosts, err := s.st.Q().ListHosts(ctx)
	if err != nil {
		return
	}
	for _, h := range hosts {
		if h.State == "enrolled" && (h.CatalogueRefreshedAt == nil || s.now().Sub(*h.CatalogueRefreshedAt) >= catalogueEach) {
			s.refreshSoon(h.FQDN)
		}
	}
}

// SnapshotQuery narrows Snapshots.
type SnapshotQuery struct {
	JobID   string
	Month   string // YYYY-MM, in the fleet's zone
	All     bool   // with tombstones
	Refresh bool   // read the repository first
	// Retention adds, for one job, why each snapshot is kept and until when a pending one stays. It asks
	// restic, with --dry-run and no lock.
	Retention bool
}

// Snapshots lists a host's snapshots, newest first (spec §5.2).
func (s *Server) Snapshots(ctx context.Context, fqdn string, in SnapshotQuery) ([]store.Snapshot, error) {
	if _, err := s.st.Q().GetHost(ctx, fqdn); err != nil {
		return nil, fmt.Errorf("host %s: %w", fqdn, err)
	}
	f := store.SnapshotFilter{JobID: in.JobID, WithGone: in.All}
	if in.JobID != "" {
		if err := config.ValidJobID(in.JobID); err != nil {
			return nil, &ValidationError{config.Problems{{Path: "job", Message: err.Error()}}}
		}
	}
	if in.Month != "" {
		m, err := time.ParseInLocation("2006-01", in.Month, s.zone)
		if err != nil {
			return nil, &ValidationError{config.Problems{{Path: "month", Message: "the month is YYYY-MM"}}}
		}
		f.From, f.Until = m, m.AddDate(0, 1, 0)
	}
	if in.Refresh {
		if err := s.RefreshCatalogue(ctx, fqdn); err != nil {
			return nil, err
		}
	}
	list, err := s.st.Q().ListSnapshots(ctx, fqdn, f)
	if err != nil {
		return nil, err
	}
	if in.Retention && in.JobID != "" {
		jr, err := s.jobRetentionNow(ctx, fqdn, in.JobID)
		if err != nil {
			return nil, err
		}
		pending := map[string]time.Time{}
		for _, p := range jr.Pending {
			pending[p.ID] = p.PrunableAt
		}
		for i := range list {
			list[i].KeptBy = jr.KeptBy[list[i].ID]
			if t, ok := pending[list[i].ID]; ok {
				list[i].PendingUntil = &t
			}
		}
	}
	if err := s.pgRestoreNames(ctx, fqdn, list); err != nil {
		return nil, err
	}
	return list, s.deletionDates(ctx, fqdn, list)
}

// pgRestoreNames fills in, for each snapshot of a PostgreSQL database's job, the database a restore beside the original
// makes (decision 055), so every client shows the name the restore will use.
func (s *Server) pgRestoreNames(ctx context.Context, fqdn string, list []store.Snapshot) error {
	head, err := s.head(ctx, s.st.Q())
	if err != nil {
		return err
	}
	jobs := head.Config.Hosts[fqdn].Jobs
	for i := range list {
		if j, ok := jobs[list[i].JobID]; ok && j.PostgreSQL != nil && !j.PostgreSQL.Globals && j.PostgreSQL.Database != "" {
			list[i].RestoreDatabase = s.pgRestoreName(j.PostgreSQL.Database, list[i].Time)
		}
	}
	return nil
}

// deletionDates fills in when deletions were asked for, and when deleted data is really gone: S3 keeps the
// old versions for the lifecycle rule's days after the prune that followed the forget (decision 031).
func (s *Server) deletionDates(ctx context.Context, fqdn string, list []store.Snapshot) error {
	dels, err := s.st.Q().ListPendingDeletions(ctx, fqdn)
	if err != nil {
		return err
	}
	asked := map[string]time.Time{}
	for _, d := range dels {
		asked[d.SnapshotID] = d.RequestedAt
	}
	var runs []store.MaintenanceRun
	h, err := s.st.Q().GetHost(ctx, fqdn)
	if err != nil {
		return err
	}
	days := noncurrentDays(h)
	for i := range list {
		sn := &list[i]
		if t, ok := asked[sn.ID]; ok {
			sn.DeletionRequestedAt = &t
		}
		if !sn.DeletedByUs || sn.GoneAt == nil || days == 0 {
			continue
		}
		if runs == nil {
			if runs, err = s.st.Q().ListMaintenanceRuns(ctx, fqdn, 1000); err != nil {
				return err
			}
		}
		// runs are newest first: the oldest prune that finished after the forget
		for _, r := range runs {
			if r.Pruned && r.FinishedAt != nil && !r.FinishedAt.Before(*sn.GoneAt) {
				sn.ReallyGoneBy = s.goneBy(*r.FinishedAt, days)
			}
		}
	}
	return nil
}

// snapshot resolves a snapshot ID or prefix of a host, reading the repository once if the catalogue does
// not know it yet. A snapshot that has gone cannot be read.
func (s *Server) snapshot(ctx context.Context, fqdn, id string) (store.Snapshot, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if !snapshotIDPattern.MatchString(id) {
		return store.Snapshot{}, &ValidationError{config.Problems{{Path: "snapshot", Message: "a snapshot ID is 8 to 64 hex digits"}}}
	}
	sn, err := s.st.Q().GetSnapshot(ctx, fqdn, id)
	if errors.Is(err, store.ErrNotFound) {
		if rerr := s.RefreshCatalogue(ctx, fqdn); rerr != nil {
			return store.Snapshot{}, rerr
		}
		sn, err = s.st.Q().GetSnapshot(ctx, fqdn, id)
	}
	if err != nil {
		return store.Snapshot{}, fmt.Errorf("snapshot %s of %s: %w", id, fqdn, err)
	}
	if sn.GoneAt != nil {
		return store.Snapshot{}, fmt.Errorf("snapshot %s of %s is no longer in the repository (gone %s): %w", sn.ShortID, fqdn, sn.GoneAt.Format(time.RFC3339), ErrNotFound)
	}
	return sn, nil
}

func page[T any](all []T, n int) ([]T, int, int) {
	pages := max(1, (len(all)+pageSize-1)/pageSize)
	n = min(max(n, 1), pages)
	from := (n - 1) * pageSize
	return all[from:min(from+pageSize, len(all))], n, pages
}

// FilesPage is one page of one directory of a snapshot: names and metadata only (spec §5.3).
type FilesPage struct {
	Snapshot string        `json:"snapshot"`
	Path     string        `json:"path"`
	Entries  []restic.Node `json:"entries"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	Pages    int           `json:"pages"`
}

// Files lists one directory of a snapshot, a page of entries at a time.
func (s *Server) Files(ctx context.Context, fqdn, id, dir string, pageNo int) (FilesPage, error) {
	sn, err := s.snapshot(ctx, fqdn, id)
	if err != nil {
		return FilesPage{}, err
	}
	if dir == "" {
		dir = "/"
	}
	if !strings.HasPrefix(dir, "/") || strings.ContainsRune(dir, 0) {
		return FilesPage{}, &ValidationError{config.Problems{{Path: "path", Message: "the path must be absolute"}}}
	}
	dir = path.Clean(dir)
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	r, cleanup, err := s.readRunner(ctx, fqdn)
	if err != nil {
		return FilesPage{}, err
	}
	defer cleanup()
	nodes, err := r.Ls(ctx, sn.ID, dir)
	if err != nil {
		return FilesPage{}, err
	}
	entries, n, pages := page(nodes, pageNo)
	return FilesPage{Snapshot: sn.ID, Path: dir, Entries: entries, Total: len(nodes), Page: n, Pages: pages}, nil
}

// ChangesView is what changed between two snapshots: counts, and the changed paths a page at a time. It
// never shows contents (spec §5.3).
type ChangesView struct {
	Snapshot string           `json:"snapshot"`
	Against  string           `json:"against"`
	Added    int              `json:"added"`
	Removed  int              `json:"removed"`
	Content  int              `json:"content_changed"`
	Metadata int              `json:"metadata_changed"`
	Type     int              `json:"type_changed"`
	Stats    restic.DiffStats `json:"stats"`
	Changes  []restic.Change  `json:"changes"`
	Total    int              `json:"total"`
	Page     int              `json:"page"`
	Pages    int              `json:"pages"`
}

// Changes compares a snapshot with an older one: by default the previous snapshot of the same job.
func (s *Server) Changes(ctx context.Context, fqdn, id, against string, pageNo int) (ChangesView, error) {
	sn, err := s.snapshot(ctx, fqdn, id)
	if err != nil {
		return ChangesView{}, err
	}
	var older store.Snapshot
	if against != "" {
		if older, err = s.snapshot(ctx, fqdn, against); err != nil {
			return ChangesView{}, err
		}
	} else {
		list, err := s.st.Q().ListSnapshots(ctx, fqdn, store.SnapshotFilter{JobID: sn.JobID, Until: sn.Time})
		if err != nil {
			return ChangesView{}, err
		}
		if len(list) == 0 {
			return ChangesView{}, fmt.Errorf("snapshot %s is the oldest of its job; give --against: %w", sn.ShortID, ErrNotFound)
		}
		older = list[0]
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	r, cleanup, err := s.readRunner(ctx, fqdn)
	if err != nil {
		return ChangesView{}, err
	}
	defer cleanup()
	changes, stats, err := r.Diff(ctx, older.ID, sn.ID)
	if err != nil {
		return ChangesView{}, err
	}
	v := ChangesView{Snapshot: sn.ID, Against: older.ID, Stats: stats, Total: len(changes)}
	for _, c := range changes {
		switch c.Modifier {
		case "+":
			v.Added++
		case "-":
			v.Removed++
		case "M":
			v.Content++
		case "U":
			v.Metadata++
		case "T":
			v.Type++
		}
	}
	v.Changes, v.Page, v.Pages = page(changes, pageNo)
	return v, nil
}

// FoundIn is one snapshot's matches of a find.
type FoundIn struct {
	Snapshot string        `json:"snapshot"`
	ShortID  string        `json:"short_id"`
	Time     time.Time     `json:"time"`
	JobID    string        `json:"job_id,omitempty"`
	Matches  []restic.Node `json:"matches"`
}

// Find looks for a file name pattern in all of a host's snapshots, newest first (spec §5.2).
func (s *Server) Find(ctx context.Context, fqdn, pattern string) ([]FoundIn, error) {
	if strings.TrimSpace(pattern) == "" || len(pattern) > 1024 || strings.ContainsRune(pattern, 0) {
		return nil, &ValidationError{config.Problems{{Path: "pattern", Message: "give a file name or a pattern such as *.conf"}}}
	}
	if _, err := s.st.Q().GetHost(ctx, fqdn); err != nil {
		return nil, fmt.Errorf("host %s: %w", fqdn, err)
	}
	fctx, cancel := context.WithTimeout(ctx, findTimeout)
	defer cancel()
	r, cleanup, err := s.readRunner(fctx, fqdn)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	found, err := r.Find(fctx, pattern)
	if errors.Is(fctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("find stopped after %s; narrow the pattern: %w", findTimeout, ErrTimeout)
	}
	if err != nil {
		return nil, err
	}
	list, err := s.st.Q().ListSnapshots(ctx, fqdn, store.SnapshotFilter{})
	if err != nil {
		return nil, err
	}
	byID := map[string]store.Snapshot{}
	for _, sn := range list {
		byID[sn.ID] = sn
	}
	out := []FoundIn{}
	for _, f := range found {
		sn, ok := byID[f.Snapshot]
		if !ok {
			sn.ShortID = f.Snapshot[:min(8, len(f.Snapshot))] // newer than the catalogue
		}
		out = append(out, FoundIn{Snapshot: f.Snapshot, ShortID: sn.ShortID, Time: sn.Time, JobID: sn.JobID, Matches: f.Matches})
	}
	return out, nil
}

// Acknowledged is what AcknowledgeOutside cleared.
type Acknowledged struct {
	Snapshots int `json:"snapshots"` // disappeared snapshots; their tombstones stay, and stay searchable
	Keys      int `json:"keys"`      // repository keys added outside Holdfast, now accepted as known
}

// AcknowledgeOutside clears a host's red banners after an admin has looked into them (decision 032).
func (s *Server) AcknowledgeOutside(ctx context.Context, a Actor, fqdn string) (Acknowledged, error) {
	if _, err := s.st.Q().GetHost(ctx, fqdn); err != nil {
		return Acknowledged{}, fmt.Errorf("host %s: %w", fqdn, err)
	}
	var got Acknowledged
	err := s.st.InTx(ctx, func(q *store.Q) error {
		var err error
		if got.Snapshots, err = q.AcknowledgeDisappeared(ctx, fqdn, s.now()); err != nil {
			return err
		}
		if got.Keys, err = q.AcknowledgeRepositoryKeys(ctx, fqdn); err != nil {
			return err
		}
		if got.Snapshots == 0 && got.Keys == 0 {
			return nil
		}
		return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "host.outside_changes_acknowledged", FQDN: fqdn,
			Detail: map[string]any{"snapshots": got.Snapshots, "keys": got.Keys}})
	})
	return got, err
}

// ErrTimeout is returned as 504 when a read ran out of time.
var ErrTimeout = errors.New("timed out")
