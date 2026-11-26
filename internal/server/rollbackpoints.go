package server

import (
	"context"
	"fmt"
	"path"
	"slices"
	"sort"
	"time"

	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// What a rollback can undo (spec §3.5b, decision 058): each moment something in a repository was deleted or
// overwritten, from S3's versions, with what went and who did it. Rolling back to just before one brings back what
// went then and since. The repository changes only when something is written or deleted, so these are the moments
// worth rolling back to; any time between two of them gives the same result.

// RollbackPoint is one moment of deletions or overwrites.
type RollbackPoint struct {
	At time.Time `json:"at"` // the first change
	To time.Time `json:"to"` // just before it: what bringing it back rolls back to
	// By is Holdfast's run that made the changes, scheduled, run_now, prune_now, delete_now or rollback, with its
	// actor; empty when none did: they were made behind Holdfast's back.
	By          string         `json:"by,omitempty"`
	Actor       string         `json:"actor,omitempty"`
	Deleted     map[string]int `json:"deleted"`     // by kind: data, index, snapshots, keys, config, other
	Overwritten map[string]int `json:"overwritten"` // by kind; restic never overwrites, so never by Holdfast's runs
	Snapshots   []string       `json:"snapshots"`   // the snapshots deleted, by ID
	Jobs        []string       `json:"jobs"`        // their jobs, from the catalogue
}

// RollbackPoints answers GET /api/v1/agents/{agent}/rollback without a time: the moments, newest first, back to the
// earliest a rollback can reach.
type RollbackPoints struct {
	FQDN    string          `json:"fqdn"`
	Oldest  time.Time       `json:"oldest"`
	Refused string          `json:"refused,omitempty"` // why no rollback can be done; then there are no points
	Points  []RollbackPoint `json:"points"`
}

// pointGap is how long a quiet spell parts two moments of changes that no run of Holdfast's explains.
const pointGap = 10 * time.Minute

// A run's changes are those from its start to its end; failing that, those a little before or after: the bucket's
// clock and the server's may differ, and a run records its end after its last change.
const (
	slackBefore = 2 * time.Second
	slackAfter  = time.Minute
)

// RollbackPointsFor lists what a rollback of fqdn's repository can undo.
func (s *Server) RollbackPointsFor(ctx context.Context, fqdn string) (RollbackPoints, error) {
	h, w, err := s.rollbackWindow(ctx, fqdn)
	if err != nil {
		return RollbackPoints{}, err
	}
	out := RollbackPoints{FQDN: fqdn, Oldest: w.oldest, Points: []RollbackPoint{}}
	switch {
	case decommissioned(h):
		out.Refused = "the host was decommissioned: its repository is deleted"
	case w.days == 0:
		out.Refused = "the bucket's lifecycle rule is not known (B011), so it is not known which old versions are still there"
	}
	if out.Refused != "" {
		return out, nil
	}
	versions, err := s.bucket.Versions(ctx, fqdn)
	if err != nil {
		return RollbackPoints{}, err
	}
	changes := storage.Changes(storage.Prefix(fqdn)+"/", versions, w.oldest)
	if len(changes) == 0 {
		return out, nil
	}
	q := s.st.Q()
	runs, err := q.ListMaintenanceRuns(ctx, fqdn, 500)
	if err != nil {
		return RollbackPoints{}, err
	}
	rbs, err := q.ListRollbacks(ctx, fqdn, 200)
	if err != nil {
		return RollbackPoints{}, err
	}
	var wins []runWindow
	now := s.now()
	end := func(t *time.Time) time.Time {
		if t == nil {
			return now
		}
		return *t
	}
	for _, r := range runs {
		wins = append(wins, runWindow{r.StartedAt, end(r.FinishedAt), r.Kind, r.Actor})
	}
	for _, r := range rbs {
		wins = append(wins, runWindow{r.RequestedAt, end(r.FinishedAt), "rollback", r.Actor})
	}
	snaps, err := q.ListSnapshots(ctx, fqdn, store.SnapshotFilter{WithGone: true})
	if err != nil {
		return RollbackPoints{}, err
	}
	jobOf := map[string]string{}
	for _, sn := range snaps {
		jobOf[sn.ID] = sn.JobID
	}
	out.Points = rollbackPoints(changes, wins, jobOf)
	return out, nil
}

// runWindow is when one of Holdfast's runs changed the repository: from its start to its end.
type runWindow struct {
	from, to   time.Time
	kind, actr string
}

// rollbackPoints puts the changes, oldest first, into moments: each run of Holdfast's that made some is one, by
// itself; the rest part where they are quiet for pointGap. It returns them newest first.
func rollbackPoints(changes []storage.Change, wins []runWindow, jobOf map[string]string) []RollbackPoint {
	owner := func(c storage.Change) int {
		if c.Overwritten {
			return -1 // restic never overwrites: not Holdfast's
		}
		for _, slack := range [][2]time.Duration{{0, 0}, {slackBefore, slackAfter}} {
			for i, w := range wins {
				if !c.At.Before(w.from.Add(-slack[0])) && !c.At.After(w.to.Add(slack[1])) {
					return i
				}
			}
		}
		return -1
	}
	var pts []RollbackPoint
	open := map[int]int{} // a run's moment, by window
	last := -1            // the newest moment no run explains
	var lastAt time.Time
	for _, c := range changes {
		i := owner(c)
		var k int
		switch {
		case i >= 0:
			if j, ok := open[i]; ok {
				k = j
			} else {
				pts = append(pts, newPoint(c.At, wins[i].kind, wins[i].actr))
				k = len(pts) - 1
				open[i] = k
			}
		case last >= 0 && c.At.Sub(lastAt) <= pointGap:
			k = last
		default:
			pts = append(pts, newPoint(c.At, "", ""))
			k = len(pts) - 1
			last = k
		}
		if i < 0 {
			lastAt = c.At
		}
		p := &pts[k]
		if c.Overwritten {
			p.Overwritten[c.Kind]++
			continue
		}
		p.Deleted[c.Kind]++
		if c.Kind == "snapshots" {
			id := path.Base(c.Key)
			p.Snapshots = append(p.Snapshots, id)
			if j := jobOf[id]; j != "" && !slices.Contains(p.Jobs, j) {
				p.Jobs = append(p.Jobs, j)
			}
		}
	}
	for i := range pts {
		sort.Strings(pts[i].Snapshots)
		sort.Strings(pts[i].Jobs)
	}
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].At.After(pts[j].At) })
	return pts
}

func newPoint(at time.Time, by, actor string) RollbackPoint {
	return RollbackPoint{At: at.UTC(), To: at.Add(-time.Second).UTC(), By: by, Actor: actor, Deleted: map[string]int{}, Overwritten: map[string]int{},
		Snapshots: []string{}, Jobs: []string{}}
}

// rollbackBounds is how far back a host's repository can be rolled back: the lifecycle rule's days, and not before
// its last password rotation.
type rollbackBounds struct {
	days         int
	oldest       time.Time
	lastRotation *time.Time
}

func (s *Server) rollbackWindow(ctx context.Context, fqdn string) (store.Host, rollbackBounds, error) {
	h, err := s.st.Q().GetHost(ctx, fqdn)
	if err != nil {
		return store.Host{}, rollbackBounds{}, fmt.Errorf("host %s: %w", fqdn, err)
	}
	var b rollbackBounds
	if h.Storage != nil {
		b.days = h.Storage.NoncurrentDays
	}
	if b.days > 0 {
		b.oldest = s.now().AddDate(0, 0, -b.days).UTC()
	}
	if last, err := s.st.Q().ListAuditFiltered(ctx, store.AuditFilter{FQDN: fqdn, Action: "host.rotated", Limit: 1}); err == nil && len(last) > 0 {
		at := last[0].At
		b.lastRotation = &at
		if at.After(b.oldest) {
			b.oldest = at.UTC()
		}
	}
	return h, b, nil
}
