package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/proto"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// MaxPollWait bounds a long poll.
const MaxPollWait = 30 * time.Second

// AgentHandler serves /agent/v1, /metrics and /healthz on the TCP listener.
func (s *Server) AgentHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+proto.Base+"/enroll", s.handleEnroll)
	mux.HandleFunc("POST "+proto.Base+"/poll", s.agentAuth(s.handlePoll))
	mux.HandleFunc("POST "+proto.Base+"/events", s.agentAuth(s.handleEvents))
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.st.Pool().Ping(r.Context()); err != nil {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, proto.ErrorResponse{Error: msg})
}

type ctxKey int

const hostKey ctxKey = 1

// agentAuth resolves the bearer token to the host it belongs to. The host identity always comes from
// here, never from a request body.
func (s *Server) agentAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "missing agent credential")
			return
		}
		fqdn, err := s.st.Q().AgentByToken(r.Context(), creds.HashToken(tok))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unknown agent credential")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), hostKey, fqdn)))
	}
}

func hostOf(r *http.Request) string { return r.Context().Value(hostKey).(string) }

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req proto.EnrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	tok, err := s.Enroll(r.Context(), req)
	if err != nil {
		// One message for every refusal; the audit log has the reason.
		writeErr(w, http.StatusForbidden, "enrollment refused: the code is invalid, expired or used, the host is unknown, "+
			"or its storage does not meet the contract; `holdfast audit --host <fqdn>` on the server says which")
		return
	}
	writeJSON(w, http.StatusOK, proto.EnrollResponse{Token: tok})
}

// Enroll redeems a code and returns the agent's token. The code is checked first. Storage is then set up
// again, idempotently, before the code is used, so a storage failure leaves the code valid for a retry.
func (s *Server) Enroll(ctx context.Context, req proto.EnrollRequest) (string, error) {
	now := s.now()
	fqdn := strings.TrimSpace(req.FQDN)
	codeHash := creds.HashToken(creds.NormaliseCode(req.Code))
	refuse := func(reason error) (string, error) {
		_ = s.st.Q().Audit(ctx, store.AuditEntry{Actor: "agent:" + fqdn, Source: "agent", Action: "agent.enroll_refused", FQDN: fqdn,
			Detail: map[string]any{"reason": reason.Error(), "hostname_fqdn": req.Facts.HostnameFQDN}})
		return "", reason
	}
	h, err := s.st.Q().GetHost(ctx, fqdn)
	if err != nil {
		return refuse(store.ErrCodeInvalid)
	}
	if h.State == "retired" {
		return refuse(errors.New("host is retired"))
	}
	// The code first: storage is touched only for someone who holds a valid code (decision 030).
	if err := s.st.Q().CheckEnrollmentCode(ctx, fqdn, codeHash, now); err != nil {
		return refuse(err)
	}
	rep, err := s.ensureStorage(ctx, fqdn)
	if err != nil {
		s.log.Error("storage set-up failed during enrollment", "host", fqdn, "err", err)
		return refuse(errors.New("storage set-up failed: " + err.Error()))
	}
	if !rep.OK() {
		s.log.Warn("enrollment refused: the host's storage does not meet the contract", "host", fqdn, "codes", rep.Codes())
		return refuse(errors.New("the host's storage does not meet the contract: " + rep.Summary()))
	}
	tok := creds.NewToken()
	err = s.st.InTx(ctx, func(q *store.Q) error {
		if err := q.RedeemEnrollmentCode(ctx, fqdn, codeHash, now); err != nil {
			return err
		}
		if err := q.EnrollAgent(ctx, fqdn, creds.HashToken(tok), now); err != nil {
			return err
		}
		detail := map[string]any{"agent_version": req.Facts.AgentVersion, "restic_version": req.Facts.ResticVersion,
			"os": req.Facts.OSID + " " + req.Facts.OSVersion, "hostname_fqdn": req.Facts.HostnameFQDN}
		if req.Facts.HostnameFQDN != "" && req.Facts.HostnameFQDN != fqdn {
			detail["warning"] = "hostname -f does not match the FQDN" // plan-m1 Q9: a warning, not a refusal
		}
		return q.Audit(ctx, store.AuditEntry{Actor: "agent:" + fqdn, Source: "agent", Action: "agent.enrolled", FQDN: fqdn, Detail: detail})
	})
	if err != nil {
		return refuse(err)
	}
	return tok, nil
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	var req proto.PollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	wait, err := time.ParseDuration(r.URL.Query().Get("wait"))
	if err != nil || wait < 0 || wait > MaxPollWait {
		wait = 25 * time.Second
	}
	resp, err := s.Poll(r.Context(), hostOf(r), req, wait)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.log.Error("poll failed", "host", hostOf(r), "err", err)
		writeErr(w, http.StatusInternalServerError, "poll failed")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// Poll records the heartbeat, then answers as soon as the agent's config, credentials or commands
// differ from what it has, or when wait has passed.
func (s *Server) Poll(ctx context.Context, fqdn string, req proto.PollRequest, wait time.Duration) (proto.PollResponse, error) {
	now := s.now()
	offset := int64(0)
	if !req.Facts.Now.IsZero() {
		offset = req.Facts.Now.Sub(now).Milliseconds()
	}
	q := s.st.Q()
	if err := q.RecordHeartbeat(ctx, fqdn, store.Heartbeat{AgentVersion: req.Facts.AgentVersion, ResticVersion: req.Facts.ResticVersion,
		OSID: req.Facts.OSID, OSVersion: req.Facts.OSVersion, HostnameFQDN: req.Facts.HostnameFQDN, Timezone: req.Facts.Timezone,
		ClockOffsetMS: offset, AppliedRevision: req.Applied.Revision, AppliedSHA256: req.Applied.SHA256, CredentialsVersion: req.CredentialsVersion}, now); err != nil {
		return proto.PollResponse{}, err
	}
	if err := q.AckCommands(ctx, fqdn, req.AckedCommands, now); err != nil {
		return proto.PollResponse{}, err
	}
	acked := map[string]bool{}
	for _, id := range req.AckedCommands {
		acked[id] = true
	}
	deadline := time.Now().Add(wait) // a long-poll waits real time, whatever the server's clock is set to
	for {
		resp, changed, err := s.pollAnswer(ctx, fqdn, req, acked)
		if err != nil {
			return proto.PollResponse{}, err
		}
		remaining := time.Until(deadline)
		if changed || remaining <= 0 {
			resp.ServerTime = s.now()
			return resp, nil
		}
		s.hub.wait(ctx, fqdn, remaining)
		if ctx.Err() != nil {
			return proto.PollResponse{}, ctx.Err()
		}
	}
}

func (s *Server) pollAnswer(ctx context.Context, fqdn string, req proto.PollRequest, acked map[string]bool) (proto.PollResponse, bool, error) {
	var resp proto.PollResponse
	changed := false
	q := s.st.Q()
	head, err := s.head(ctx, q)
	if err != nil {
		return resp, false, err
	}
	hc := s.renderHost(head.Config, fqdn)
	if rev, _, err := q.HostDesired(ctx, fqdn); err == nil {
		hc.Revision = rev
	}
	if hc.SHA256 != req.Applied.SHA256 {
		resp.Config, changed = &hc, true
	}
	c, err := q.GetCredentials(ctx, fqdn)
	if err != nil {
		return resp, false, err
	}
	if c.Version != req.CredentialsVersion {
		pw, err1 := s.seal.Open(c.RepoPassword, fqdn)
		sec, err2 := s.seal.Open(c.S3Secret, fqdn)
		if err := errors.Join(err1, err2); err != nil {
			return resp, false, err
		}
		resp.Credentials = &proto.Credentials{Version: c.Version, RepoPassword: pw, S3AccessKeyID: c.S3AccessKeyID, S3SecretAccessKey: sec}
		changed = true
	}
	cmds, err := q.PendingCommands(ctx, fqdn)
	if err != nil {
		return resp, false, err
	}
	for _, c := range cmds {
		if !acked[c.ID] {
			resp.Commands = append(resp.Commands, proto.Command{ID: c.ID, Kind: c.Kind, JobID: c.JobID})
			changed = true
		}
	}
	return resp, changed, nil
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	var req proto.EventsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	acked, err := s.Events(r.Context(), hostOf(r), req.Events)
	if err != nil {
		s.log.Error("storing events failed", "host", hostOf(r), "err", err)
		writeErr(w, http.StatusInternalServerError, "storing events failed")
		return
	}
	writeJSON(w, http.StatusOK, proto.EventsResponse{Acked: acked})
}

// Events stores each event once and acknowledges all of them, including ones stored before.
func (s *Server) Events(ctx context.Context, fqdn string, evs []proto.Event) ([]string, error) {
	acked := []string{}
	newSnapshot := false
	for _, e := range evs {
		if e.ID == "" {
			continue
		}
		err := s.st.InTx(ctx, func(q *store.Q) error {
			_, err := q.InsertEvent(ctx, store.Event{FQDN: fqdn, ID: e.ID, Kind: e.Kind, JobID: e.JobID, CommandID: e.CommandID, Status: e.Status,
				ScheduledFor: e.ScheduledFor, SpreadDelayS: e.SpreadDelayS, Started: e.Started, Finished: e.Finished, Revision: e.Revision,
				SnapshotID: e.SnapshotID, FilesNew: e.FilesNew, FilesChanged: e.FilesChanged, BytesAdded: e.BytesAdded,
				TotalFiles: e.TotalFiles, TotalBytes: e.TotalBytes, ResticExit: e.ResticExit, Error: e.Error})
			return err
		})
		if err != nil {
			return acked, err
		}
		acked = append(acked, e.ID)
		if e.SnapshotID != "" {
			newSnapshot = true
		}
	}
	if newSnapshot {
		s.refreshSoon(fqdn) // the catalogue follows each new snapshot (plan-m2 §3)
	}
	return acked, nil
}
