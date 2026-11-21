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
// In M1 it is served on a root-only Unix socket, and the actor is the socket peer's user
// (plan-m1 Q2). M4 serves the same handler over HTTPS with OIDC, and adds role checks here.

const actorKey ctxKey = 2

func actorOf(r *http.Request) Actor {
	if a, ok := r.Context().Value(actorKey).(Actor); ok {
		return a
	}
	return Actor{Name: "unknown", Source: "api"}
}

// PeerActor returns a ConnContext hook that records the peer's Unix user as the actor.
func PeerActor() func(ctx context.Context, c net.Conn) context.Context {
	return func(ctx context.Context, c net.Conn) context.Context {
		a := Actor{Name: "local:unknown", Source: "cli"}
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

// APIHandler serves /api/v1. local adds the endpoints that exist only on the root-only socket.
func (s *Server) APIHandler(local bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", s.apiStatus)
	mux.HandleFunc("GET /api/v1/hosts", s.apiHosts)
	mux.HandleFunc("POST /api/v1/hosts", s.apiAddHost)
	mux.HandleFunc("GET /api/v1/hosts/{fqdn}", s.apiHost)
	mux.HandleFunc("GET /api/v1/hosts/{fqdn}/events", s.apiHostEvents)
	mux.HandleFunc("POST /api/v1/hosts/{fqdn}/enrollment-code", s.apiEnrollmentCode)
	mux.HandleFunc("PUT /api/v1/hosts/{fqdn}/s3-key", s.apiSetS3Key)
	mux.HandleFunc("GET /api/v1/hosts/{fqdn}/snapshots", s.apiSnapshots)
	mux.HandleFunc("GET /api/v1/hosts/{fqdn}/snapshots/{id}/files", s.apiFiles)
	mux.HandleFunc("GET /api/v1/hosts/{fqdn}/snapshots/{id}/changes", s.apiChanges)
	mux.HandleFunc("GET /api/v1/hosts/{fqdn}/find", s.apiFind)
	mux.HandleFunc("POST /api/v1/hosts/{fqdn}/acknowledge", s.apiAcknowledge)
	mux.HandleFunc("POST /api/v1/hosts/{fqdn}/snapshots/{id}/restore", s.apiRestore)
	mux.HandleFunc("GET /api/v1/restores", s.apiRestores)
	mux.HandleFunc("GET /api/v1/search", s.apiSearch)
	mux.HandleFunc("PUT /api/v1/hosts/{fqdn}/jobs/{job}", s.apiSetJob)
	mux.HandleFunc("POST /api/v1/hosts/{fqdn}/jobs/{job}/check", s.apiCheckJob)
	mux.HandleFunc("POST /api/v1/hosts/{fqdn}/jobs/{job}/keep-longer", s.apiKeepLonger)
	// Retention (plan-m3): what waits, the daily run, Prune now, Delete snapshot, and the past prunes.
	mux.HandleFunc("GET /api/v1/hosts/{fqdn}/pending", s.apiPending)
	mux.HandleFunc("POST /api/v1/hosts/{fqdn}/retention/run", s.apiRunRetention)
	mux.HandleFunc("GET /api/v1/hosts/{fqdn}/prune-now", s.apiPruneNowPlan)
	mux.HandleFunc("POST /api/v1/hosts/{fqdn}/prune-now", s.apiPruneNow)
	mux.HandleFunc("POST /api/v1/hosts/{fqdn}/snapshots/{id}/delete", s.apiDeleteSnapshot)
	mux.HandleFunc("DELETE /api/v1/hosts/{fqdn}/snapshots/{id}/delete", s.apiCancelDeletion)
	mux.HandleFunc("GET /api/v1/hosts/{fqdn}/prunes", s.apiPrunes)
	mux.HandleFunc("GET /api/v1/prunes", s.apiPrunes)
	mux.HandleFunc("POST /api/v1/hosts/{fqdn}/jobs/{job}/run", s.apiRunNow)
	mux.HandleFunc("GET /api/v1/revisions", s.apiRevisions)
	mux.HandleFunc("GET /api/v1/revisions/{n}", s.apiRevision)
	mux.HandleFunc("GET /api/v1/revisions/{a}/diff/{b}", s.apiDiff)
	mux.HandleFunc("GET /api/v1/audit", s.apiAudit)
	mux.HandleFunc("POST /api/v1/maintenance/check", s.apiCheck)
	if local {
		// Credentials leave the server only here, to root on the server (spec §2.3; a listed exception
		// to UI = API = CLI).
		mux.HandleFunc("POST /api/v1/escrow/export", s.apiEscrow)
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
	hosts, err := s.Hosts(r.Context())
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hosts)
}

func (s *Server) apiHost(w http.ResponseWriter, r *http.Request) {
	h, err := s.Host(r.Context(), r.PathValue("fqdn"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
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
		All: qv.Get("all") == "1", Refresh: qv.Get("refresh") == "1"})
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
	res, err := s.Search(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiRestores(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	v, err := s.Restores(r.Context(), limit)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) apiAcknowledge(w http.ResponseWriter, r *http.Request) {
	got, err := s.AcknowledgeOutside(r.Context(), actorOf(r), r.PathValue("fqdn"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (s *Server) apiSetS3Key(w http.ResponseWriter, r *http.Request) {
	var in SetS3KeyInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.SetS3Key(r.Context(), actorOf(r), r.PathValue("fqdn"), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
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
	list, err := s.Prunes(r.Context(), r.PathValue("fqdn"), r.URL.Query().Get("all") == "1", limit)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
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
	revs, err := s.st.Q().ListRevisions(r.Context(), limit)
	if err != nil {
		apiError(w, err)
		return
	}
	if revs == nil {
		revs = []store.Revision{}
	}
	writeJSON(w, http.StatusOK, revs)
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
	rev, err := s.st.Q().GetRevision(r.Context(), n)
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
	changes, err := s.Diff(r.Context(), a, b)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": a, "to": b, "changes": changes})
}

func (s *Server) apiAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	list, err := s.st.Q().ListAudit(r.Context(), r.URL.Query().Get("host"), limit)
	if err != nil {
		apiError(w, err)
		return
	}
	if list == nil {
		list = []store.AuditEntry{}
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
