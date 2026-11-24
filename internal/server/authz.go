package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/kabsdk/holdfast/internal/access"
	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Who may do what (spec §6.2). Every /api/v1 route declares the permission it needs on the host
// in its path, and the guard checks it before the handler runs. Routes without a host in their path check in
// the handler, or cut their answer down to the hosts the actor can see (decision 033).

// Actor is who makes a request: a principal with its groups (internal/access). On the root socket it is
// admin on every host (decision 034).
type Actor = access.Principal

// ErrForbidden is answered with 403.
var ErrForbidden = errors.New("forbidden")

// ForbiddenError names the permission that was missing.
type ForbiddenError struct {
	Perm string
	FQDN string // empty: the permission is needed on every host
}

func (e *ForbiddenError) Error() string {
	if e.FQDN == "" {
		return fmt.Sprintf("you need %s on every host", e.Perm)
	}
	return fmt.Sprintf("you need %s on %s", e.Perm, e.FQDN)
}

func (e *ForbiddenError) Is(target error) bool { return target == ErrForbidden }

// accessBindings returns the role bindings in force.
func (s *Server) accessBindings(ctx context.Context) ([]config.Binding, error) {
	head, err := s.head(ctx, s.st.Q())
	return head.Config.Access.Bindings, err
}

// require checks that a may do perm on a host. A host a cannot see answers "not found", like one that does
// not exist, so a host's name cannot be probed. A refusal is audited.
func (s *Server) require(ctx context.Context, a Actor, perm, fqdn string) error {
	b, err := s.accessBindings(ctx)
	if err != nil {
		return err
	}
	if !a.Sees(fqdn, b) {
		return fmt.Errorf("host %s: %w", fqdn, ErrNotFound)
	}
	if !a.Can(perm, fqdn, b) {
		s.auditDenied(ctx, a, perm, fqdn)
		return &ForbiddenError{Perm: perm, FQDN: fqdn}
	}
	return nil
}

// allowed checks perm for checks that depend on a request's content, such as a restore's target, after the
// route's guard has let the request in. It never says "not found": the guard already did that.
func (s *Server) allowed(ctx context.Context, a Actor, perm, fqdn string) error {
	b, err := s.accessBindings(ctx)
	if err != nil {
		return err
	}
	if !a.Can(perm, fqdn, b) {
		s.auditDenied(ctx, a, perm, fqdn)
		return &ForbiddenError{Perm: perm, FQDN: fqdn}
	}
	return nil
}

// requireEverywhere checks that a is admin on every host: for what is not about one host, such as the
// bindings, which could otherwise give someone more than their own hosts.
func (s *Server) requireEverywhere(ctx context.Context, a Actor, perm string) error {
	b, err := s.accessBindings(ctx)
	if err != nil {
		return err
	}
	if !a.AdminEverywhere(b) {
		s.auditDenied(ctx, a, perm, "")
		return &ForbiddenError{Perm: perm}
	}
	return nil
}

func (s *Server) auditDenied(ctx context.Context, a Actor, perm, fqdn string) {
	_ = s.st.Q().Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "access.denied", FQDN: fqdn,
		Detail: map[string]any{"permission": perm}})
}

// Route is one /api/v1 endpoint. The parity test reads them.
type Route struct {
	Method string
	Path   string
	// Perm is checked on the {fqdn} in Path before the handler runs. Empty: the handler checks, or cuts its
	// answer down to the actor's hosts.
	Perm string
	// Local routes exist only on the root socket (spec §3.3).
	Local bool
	h     http.HandlerFunc
}

// Routes lists every /api/v1 endpoint.
func (s *Server) Routes() []Route {
	r := func(method, path, perm string, h http.HandlerFunc) Route {
		return Route{Method: method, Path: path, Perm: perm, h: h}
	}
	return []Route{
		r("GET", "/api/v1/status", "", s.apiStatus),
		r("GET", "/api/v1/agents", "", s.apiHosts),
		r("POST", "/api/v1/agents", "", s.apiAddHost),           // config.storage on the new FQDN, in AddHost
		r("POST", "/api/v1/agents/import", "", s.apiImportHost), // admin everywhere, in ImportHost
		r("GET", "/api/v1/agents/{fqdn}", access.View, s.apiHost),
		r("DELETE", "/api/v1/agents/{fqdn}", access.HostRemove, s.apiRemoveHost),
		r("DELETE", "/api/v1/agents/{fqdn}/jobs/{job}", access.HostRemove, s.apiRemoveJob),
		r("GET", "/api/v1/agents/{fqdn}/events", access.View, s.apiHostEvents),
		r("GET", "/api/v1/agents/{fqdn}/days", access.View, s.apiHostDays),
		r("POST", "/api/v1/agents/{fqdn}/enrollment-code", access.ConfigStorage, s.apiEnrollmentCode),
		r("POST", "/api/v1/agents/{fqdn}/rotate", access.ConfigStorage, s.apiRotate),
		r("POST", "/api/v1/agents/{fqdn}/decommission", access.HostDecommission, s.apiDecommission),
		r("DELETE", "/api/v1/agents/{fqdn}/decommission", access.HostDecommission, s.apiCancelDecommission),
		r("GET", "/api/v1/agents/{fqdn}/rollback", access.RepositoryRollback, s.apiRollbackPreview),
		r("POST", "/api/v1/agents/{fqdn}/rollback", access.RepositoryRollback, s.apiRollback),
		r("GET", "/api/v1/agents/{fqdn}/rollbacks", access.View, s.apiRollbacks),
		r("GET", "/api/v1/agents/{fqdn}/snapshots", access.View, s.apiSnapshots),
		r("GET", "/api/v1/agents/{fqdn}/snapshots/{id}/files", access.Browse, s.apiFiles),
		r("GET", "/api/v1/agents/{fqdn}/snapshots/{id}/changes", access.Browse, s.apiChanges),
		r("GET", "/api/v1/agents/{fqdn}/find", access.Browse, s.apiFind),
		r("POST", "/api/v1/agents/{fqdn}/acknowledge", access.Maintenance, s.apiAcknowledge),
		r("POST", "/api/v1/agents/{fqdn}/snapshots/{id}/restore", access.RestoreNewDir, s.apiRestore), // in place: in Restore
		r("POST", "/api/v1/agents/{fqdn}/snapshots/{id}/restore-pair", access.RestoreNewDir, s.apiRestorePair),
		r("GET", "/api/v1/agents/{fqdn}/restore-pair", access.Browse, s.apiRestorePairStatus),
		r("GET", "/api/v1/restores", "", s.apiRestores),
		r("GET", "/api/v1/search", "", s.apiSearch),
		r("PUT", "/api/v1/agents/{fqdn}/jobs/{job}", access.ConfigJobs, s.apiSetJob), // R104: retention.cut, in SetJob
		r("POST", "/api/v1/agents/{fqdn}/jobs", access.ConfigJobs, s.apiSetJobs),     // several jobs, one revision
		// PostgreSQL connections (decision 055): they hold credentials, so setting one is config.storage, an admin's.
		r("GET", "/api/v1/agents/{fqdn}/postgresql", access.View, s.apiConnections),
		r("PUT", "/api/v1/agents/{fqdn}/postgresql/{name}", access.ConfigStorage, s.apiSetConnection),
		r("DELETE", "/api/v1/agents/{fqdn}/postgresql/{name}", access.ConfigStorage, s.apiRemoveConnection),
		r("POST", "/api/v1/agents/{fqdn}/postgresql/{name}/test", access.ConfigJobs, s.apiTestConnection),
		r("POST", "/api/v1/agents/{fqdn}/jobs/{job}/check", access.ConfigJobs, s.apiCheckJob),
		r("POST", "/api/v1/agents/{fqdn}/jobs/{job}/keep-longer", access.ConfigJobs, s.apiKeepLonger),
		r("POST", "/api/v1/agents/{fqdn}/jobs/{job}/exclude", access.ConfigJobs, s.apiExclude(true)), // off-site (vault.go)
		r("POST", "/api/v1/agents/{fqdn}/jobs/{job}/include", access.ConfigJobs, s.apiExclude(false)),
		r("POST", "/api/v1/agents/{fqdn}/jobs/{job}/match-primary", access.ConfigJobs, s.apiMatchPrimary), // R104: retention.cut, in SetJob
		r("POST", "/api/v1/agents/{fqdn}/jobs/{job}/run", access.BackupRun, s.apiRunNow),
		r("GET", "/api/v1/agents/{fqdn}/pending", access.View, s.apiPending),
		r("POST", "/api/v1/agents/{fqdn}/retention/run", access.RetentionCut, s.apiRunRetention),
		r("GET", "/api/v1/agents/{fqdn}/prune-now", access.RetentionCut, s.apiPruneNowPlan),
		r("POST", "/api/v1/agents/{fqdn}/prune-now", access.RetentionCut, s.apiPruneNow),
		r("POST", "/api/v1/agents/{fqdn}/snapshots/{id}/delete", access.SnapshotDelete, s.apiDeleteSnapshot),
		r("DELETE", "/api/v1/agents/{fqdn}/snapshots/{id}/delete", access.SnapshotDelete, s.apiCancelDeletion),
		r("GET", "/api/v1/agents/{fqdn}/prunes", access.View, s.apiPrunes),
		r("GET", "/api/v1/prunes", "", s.apiPrunes),
		r("GET", "/api/v1/revisions", "", s.apiRevisions),
		r("GET", "/api/v1/revisions/{n}", "", s.apiRevision),
		r("GET", "/api/v1/revisions/{a}/diff/{b}", "", s.apiDiff),
		r("GET", "/api/v1/audit", "", s.apiAudit),
		r("POST", "/api/v1/maintenance/check", "", s.apiCheck), // maintenance on each host it touches
		r("GET", "/api/v1/me", "", s.apiMe),
		r("GET", "/api/v1/settings", "", s.apiSettings),
		r("GET", "/api/v1/retention-cuts", "", s.apiRetentionCuts),   // cut down to the actor's hosts
		r("GET", "/api/v1/access", "", s.apiAccess),                  // admin everywhere, in Access
		r("PUT", "/api/v1/access", "", s.apiSetAccess),               // admin everywhere, in SetAccess
		r("GET", "/api/v1/sessions", "", s.apiSessions),              // admin everywhere
		r("DELETE", "/api/v1/sessions/{user}", "", s.apiEndSessions), // admin everywhere
		r("GET", "/api/v1/agents/{fqdn}/offsite", access.View, s.apiOffsiteCopies),
		r("GET", "/api/v1/exports", "", s.apiExports),
		r("POST", "/api/v1/exports", "", s.apiExportNow),      // admin everywhere, in ExportNow
		r("POST", "/api/v1/notify/test", "", s.apiNotifyTest), // admin everywhere, in NotifyTest
		// Root's one-time link into the UI: on the root socket only.
		{Method: "POST", Path: "/api/v1/ui-login", Local: true, h: s.apiUILogin},
	}
}

// guard runs the route's check, then its handler.
func (s *Server) guard(rt Route) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rt.Perm != "" {
			if err := s.require(r.Context(), actorOf(r), rt.Perm, r.PathValue("fqdn")); err != nil {
				apiError(w, err)
				return
			}
		}
		rt.h(w, r)
	}
}

/* ---------------- answers cut down to the actor's hosts ---------------- */

// viewer is an actor with the bindings in force, for cutting answers down.
type viewer struct {
	a Actor
	b []config.Binding
}

func (s *Server) viewerOf(ctx context.Context, a Actor) (viewer, error) {
	b, err := s.accessBindings(ctx)
	return viewer{a, b}, err
}

func (v viewer) sees(fqdn string) bool        { return v.a.Sees(fqdn, v.b) }
func (v viewer) can(perm, fqdn string) bool   { return v.a.Can(perm, fqdn, v.b) }
func (v viewer) everywhere() bool             { return v.a.AdminEverywhere(v.b) }
func (v viewer) actions(fqdn string) []string { return v.a.On(fqdn, v.b) }
func keep[T any](xs []T, ok func(T) bool) []T {
	return slices.DeleteFunc(xs, func(x T) bool { return !ok(x) })
}

func (v viewer) hosts(hs []HostView) []HostView {
	hs = keep(hs, func(h HostView) bool { return v.sees(h.FQDN) })
	for i := range hs {
		hs[i].Actions = v.actions(hs[i].FQDN)
	}
	return hs
}

func (v viewer) search(r SearchResult) SearchResult {
	r.Hosts = keep(r.Hosts, v.sees)
	r.Jobs = keep(r.Jobs, func(j SearchJob) bool { return v.sees(j.FQDN) })
	r.Snapshots = keep(r.Snapshots, func(sn SearchSnapshot) bool { return v.sees(sn.FQDN) })
	return r
}

// restores keeps the restores of the actor's hosts. What was left out or restored only are file names, so
// they need browse.
func (v viewer) restores(rv RestoresView) RestoresView {
	rv.Queued = keep(rv.Queued, func(c store.Command) bool { return v.sees(c.FQDN) })
	for i, c := range rv.Queued {
		if !v.can(access.Browse, c.FQDN) && len(c.Payload) > 0 {
			var spec proto.RestoreSpec
			if json.Unmarshal(c.Payload, &spec) == nil {
				spec.Include, spec.Exclude = nil, nil
				rv.Queued[i].Payload, _ = json.Marshal(spec)
			}
		}
	}
	rv.Done = keep(rv.Done, func(e store.Event) bool { return v.sees(e.FQDN) })
	for i, e := range rv.Done {
		if !v.can(access.Browse, e.FQDN) {
			d := map[string]string{}
			for k, val := range e.Detail {
				if k != "include" && k != "exclude" {
					d[k] = val
				}
			}
			rv.Done[i].Detail = d
		}
	}
	return rv
}

func (v viewer) prunes(ps []PruneEntry) []PruneEntry {
	return keep(ps, func(p PruneEntry) bool { return v.sees(p.FQDN) })
}

// config cuts a revision's config down to the actor's hosts. The bindings are only for admins everywhere.
func (v viewer) config(d config.Document) config.Document {
	if v.everywhere() {
		return d
	}
	d = d.Clone()
	for fqdn := range d.Hosts {
		if !v.sees(fqdn) {
			delete(d.Hosts, fqdn)
		}
	}
	d.Access = config.Access{Bindings: []config.Binding{}}
	return d
}

// changes keeps the changes to the actor's hosts. An FQDN holds dots, so a change's host is found among the
// hosts of the two revisions compared (fqdns): the longest one its path starts with.
func (v viewer) changes(cs []Change, fqdns []string) []Change {
	if v.everywhere() {
		return cs
	}
	return keep(cs, func(c Change) bool {
		host := ""
		for _, f := range fqdns {
			p := "hosts." + f
			if (c.Path == p || strings.HasPrefix(c.Path, p+".")) && len(f) > len(host) {
				host = f
			}
		}
		return host != "" && v.sees(host)
	})
}

// touched lists the hosts whose config differs between two documents.
func touched(before, after config.Document) []string {
	var out []string
	for fqdn, h := range after.Hosts {
		if old, ok := before.Hosts[fqdn]; !ok || !reflect.DeepEqual(old, h) {
			out = append(out, fqdn)
		}
	}
	for fqdn := range before.Hosts {
		if _, ok := after.Hosts[fqdn]; !ok {
			out = append(out, fqdn)
		}
	}
	sort.Strings(out)
	return out
}

// visibleRevisions returns up to limit revisions, newest first, that changed a host the actor can see. An
// admin everywhere sees them all.
func (s *Server) visibleRevisions(ctx context.Context, v viewer, limit int) ([]store.Revision, error) {
	q := s.st.Q()
	if v.everywhere() {
		return q.ListRevisions(ctx, limit)
	}
	out := []store.Revision{}
	head, err := s.head(ctx, q)
	if err != nil || head.Number == 0 {
		return out, err
	}
	next := head
	for n := head.Number - 1; n >= 0 && len(out) < limit; n-- {
		prev := store.Revision{Config: config.New()}
		if n > 0 {
			if prev, err = q.GetRevision(ctx, n); err != nil {
				return nil, err
			}
		}
		if slices.ContainsFunc(touched(prev.Config, next.Config), v.sees) {
			r := next
			r.Config = config.Document{}
			out = append(out, r)
		}
		next = prev
	}
	return out, nil
}

// revisionVisible says whether revision n changed a host the actor can see.
func (s *Server) revisionVisible(ctx context.Context, v viewer, rev store.Revision) (bool, error) {
	if v.everywhere() {
		return true, nil
	}
	prev := store.Revision{Config: config.New()}
	if rev.Number > 1 {
		var err error
		if prev, err = s.st.Q().GetRevision(ctx, rev.Number-1); err != nil {
			return false, err
		}
	}
	return slices.ContainsFunc(touched(prev.Config, rev.Config), v.sees), nil
}
