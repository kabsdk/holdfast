package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/user"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/kabsdk/holdfast/internal/server/store"
)

// The user-facing API, /api/v1. The CLI and the UI are thin clients of it (decision 028).
//
// It is served on the root-only Unix socket, where the actor is the socket peer's user and admin on every
// host (decision 034), and on the UI listener, where the actor comes from a session (session.go). Every
// route's permission is in Routes (authz.go).

const actorKey ctxKey = 2

// actorOf returns the request's actor. A request without one, which no listener sends, may do nothing.
func actorOf(r *http.Request) Actor {
	if a, ok := r.Context().Value(actorKey).(Actor); ok {
		return a
	}
	return Actor{Name: "unknown", Source: "api"}
}

// WithActor returns ctx carrying the actor.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey, a)
}

// PeerActor returns a ConnContext hook that records the peer's Unix user as the actor. The socket is
// root-only, and root is admin on every host (decision 034).
func PeerActor() func(ctx context.Context, c net.Conn) context.Context {
	return func(ctx context.Context, c net.Conn) context.Context {
		a := Actor{Name: "local:unknown", Source: "cli", Everywhere: true}
		if uc, ok := c.(*net.UnixConn); ok {
			if raw, err := uc.SyscallConn(); err == nil {
				_ = raw.Control(func(fd uintptr) {
					if cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); err == nil {
						name := strconv.Itoa(int(cred.Uid))
						if u, err := user.LookupId(name); err == nil {
							name = u.Username
						}
						a.Name = "local:" + name
					}
				})
			}
		}
		return context.WithValue(ctx, actorKey, a)
	}
}

// APIHandler serves /api/v1. local adds the endpoints that exist only on the root-only socket: escrow
// export, by which credentials leave the server, to root on the server only (spec §2.3 and §3.3).
func (s *Server) APIHandler(local bool) http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.Routes() {
		if rt.Local && !local {
			continue
		}
		mux.HandleFunc(rt.Method+" "+rt.Path, s.guard(rt))
	}
	if !s.set.IsVault() {
		mux.HandleFunc("/api/v1/offsite/", s.serveOffsite(local)) // the off-site instance's API (offsite.go)
	}
	return mux
}

// apiError maps an error to its status code.
func apiError(w http.ResponseWriter, err error) {
	var ve *ValidationError
	var se *StorageError
	var ce *ChecksError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": ve.Error(), "problems": ve.Problems})
	case errors.As(err, &ce):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": ce.Error(), "findings": ce.Findings, "blocking": ce.Blocking})
	case errors.As(err, &se):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": se.Error(), "storage": se.Report})
	case errors.Is(err, ErrForbidden):
		writeErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrTimeout):
		writeErr(w, http.StatusGatewayTimeout, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return false
	}
	return true
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	head, err := s.head(r.Context(), s.st.Q())
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"head_revision": head.Number, "node": s.set.NodeName, "default_zone": s.set.DefaultZone, "actor": actorOf(r).Name})
}

func (s *Server) apiHosts(w http.ResponseWriter, r *http.Request) {
	v, err := s.viewerOf(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	hosts, err := s.Hosts(r.Context())
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v.hosts(hosts))
}

func (s *Server) apiHost(w http.ResponseWriter, r *http.Request) {
	v, err := s.viewerOf(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	h, err := s.Host(r.Context(), r.PathValue("fqdn"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v.hosts([]HostView{h})[0])
}

func (s *Server) apiHostEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	evs, err := s.HostEvents(r.Context(), r.PathValue("fqdn"), limit)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, evs)
}

func (s *Server) apiAddHost(w http.ResponseWriter, r *http.Request) {
	var in AddHostInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.AddHost(r.Context(), actorOf(r), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) apiEnrollmentCode(w http.ResponseWriter, r *http.Request) {
	e, err := s.IssueEnrollmentCode(r.Context(), actorOf(r), r.PathValue("fqdn"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (s *Server) apiSnapshots(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	list, err := s.Snapshots(r.Context(), r.PathValue("fqdn"), SnapshotQuery{JobID: qv.Get("job"), Month: qv.Get("month"),
		All: qv.Get("all") == "1", Refresh: qv.Get("refresh") == "1", Retention: qv.Get("retention") == "1"})
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) apiFiles(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	p, err := s.Files(r.Context(), r.PathValue("fqdn"), r.PathValue("id"), r.URL.Query().Get("path"), n)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) apiChanges(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	v, err := s.Changes(r.Context(), r.PathValue("fqdn"), r.PathValue("id"), r.URL.Query().Get("against"), n)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) apiFind(w http.ResponseWriter, r *http.Request) {
	found, err := s.Find(r.Context(), r.PathValue("fqdn"), r.URL.Query().Get("pattern"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, found)
}

func (s *Server) apiRestore(w http.ResponseWriter, r *http.Request) {
	var in RestoreInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.Restore(r.Context(), actorOf(r), r.PathValue("fqdn"), r.PathValue("id"), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) apiSearch(w http.ResponseWriter, r *http.Request) {
	v, err := s.viewerOf(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	res, err := s.Search(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v.search(res))
}

func (s *Server) apiRestores(w http.ResponseWriter, r *http.Request) {
	v, err := s.viewerOf(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rv, err := s.Restores(r.Context(), limit)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v.restores(rv))
}

func (s *Server) apiAcknowledge(w http.ResponseWriter, r *http.Request) {
	got, err := s.AcknowledgeOutside(r.Context(), actorOf(r), r.PathValue("fqdn"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (s *Server) apiSetJob(w http.ResponseWriter, r *http.Request) {
	var in SetJobInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.SetJob(r.Context(), actorOf(r), r.PathValue("fqdn"), r.PathValue("job"), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiCheckJob(w http.ResponseWriter, r *http.Request) {
	var in JobCheckInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.CheckJob(r.Context(), r.PathValue("fqdn"), r.PathValue("job"), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiKeepLonger(w http.ResponseWriter, r *http.Request) {
	var in KeepLongerInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.KeepLonger(r.Context(), actorOf(r), r.PathValue("fqdn"), r.PathValue("job"), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiPending(w http.ResponseWriter, r *http.Request) {
	p, err := s.Pending(r.Context(), r.PathValue("fqdn"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) apiRunRetention(w http.ResponseWriter, r *http.Request) {
	res, err := s.RunRetention(r.Context(), actorOf(r), r.PathValue("fqdn"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) apiPruneNowPlan(w http.ResponseWriter, r *http.Request) {
	p, err := s.PruneNowPlan(r.Context(), r.PathValue("fqdn"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) apiPruneNow(w http.ResponseWriter, r *http.Request) {
	var in PruneNowInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.PruneNow(r.Context(), actorOf(r), r.PathValue("fqdn"), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) apiDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	var in DeleteSnapshotInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.DeleteSnapshot(r.Context(), actorOf(r), r.PathValue("fqdn"), r.PathValue("id"), in)
	if err != nil {
		apiError(w, err)
		return
	}
	status := http.StatusOK
	if res.Run != 0 {
		status = http.StatusAccepted
	}
	writeJSON(w, status, res)
}

func (s *Server) apiCancelDeletion(w http.ResponseWriter, r *http.Request) {
	if err := s.CancelDeletion(r.Context(), actorOf(r), r.PathValue("fqdn"), r.PathValue("id")); err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": r.PathValue("id")})
}

func (s *Server) apiPrunes(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	v, err := s.viewerOf(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	list, err := s.Prunes(r.Context(), r.PathValue("fqdn"), r.URL.Query().Get("all") == "1", limit)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v.prunes(list))
}

func (s *Server) apiRunNow(w http.ResponseWriter, r *http.Request) {
	id, err := s.RunNow(r.Context(), actorOf(r), r.PathValue("fqdn"), r.PathValue("job"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"command": id})
}

func (s *Server) apiRevisions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	v, err := s.viewerOf(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	revs, err := s.visibleRevisions(r.Context(), v, limit)
	if err != nil {
		apiError(w, err)
		return
	}
	if revs == nil {
		revs = []store.Revision{}
	}
	writeJSON(w, http.StatusOK, revs)
}

// visibleRevision returns revision n with its config cut down, or not found if it changed none of the
// actor's hosts.
func (s *Server) visibleRevision(r *http.Request, v viewer, n int64) (store.Revision, error) {
	rev, err := s.st.Q().GetRevision(r.Context(), n)
	if err != nil {
		return rev, err
	}
	if ok, err := s.revisionVisible(r.Context(), v, rev); err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("revision %d: %w", n, store.ErrNotFound)
		}
		return store.Revision{}, err
	}
	rev.Config = v.config(rev.Config)
	return rev, nil
}

func pathInt(r *http.Request, name string) (int64, error) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s %q is not a revision number: %w", name, r.PathValue(name), store.ErrNotFound)
	}
	return n, nil
}

func (s *Server) apiRevision(w http.ResponseWriter, r *http.Request) {
	n, err := pathInt(r, "n")
	if err != nil {
		apiError(w, err)
		return
	}
	v, err := s.viewerOf(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	rev, err := s.visibleRevision(r, v, n)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": rev, "config": rev.Config})
}

func (s *Server) apiDiff(w http.ResponseWriter, r *http.Request) {
	a, errA := pathInt(r, "a")
	b, errB := pathInt(r, "b")
	if err := errors.Join(errA, errB); err != nil {
		apiError(w, err)
		return
	}
	v, err := s.viewerOf(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	changes, err := s.Diff(r.Context(), a, b)
	if err != nil {
		apiError(w, err)
		return
	}
	var fqdns []string
	for _, n := range []int64{a, b} {
		if rev, err := s.st.Q().GetRevision(r.Context(), n); err == nil {
			for f := range rev.Config.Hosts {
				fqdns = append(fqdns, f)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": a, "to": b, "changes": v.changes(changes, fqdns)})
}

func (s *Server) apiAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	v, err := s.viewerOf(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	f := store.AuditFilter{FQDN: r.URL.Query().Get("host"), Action: r.URL.Query().Get("action"), Limit: limit}
	if before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64); err == nil {
		f.Before = before
	}
	if !v.everywhere() {
		// Only the actor's hosts, and nothing that belongs to no host (decision 033).
		hosts, err := s.st.Q().ListHosts(r.Context())
		if err != nil {
			apiError(w, err)
			return
		}
		f.Hosts = []string{}
		for _, h := range hosts {
			if v.sees(h.FQDN) {
				f.Hosts = append(f.Hosts, h.FQDN)
			}
		}
	}
	list, err := s.st.Q().ListAuditFiltered(r.Context(), f)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) apiCheck(w http.ResponseWriter, r *http.Request) {
	res := s.RunChecks(r.Context(), actorOf(r))
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiEscrow(w http.ResponseWriter, r *http.Request) {
	b, err := s.EscrowExport(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}
