package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/gateway"
	"github.com/kabsdk/holdfast/internal/pin"
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
	if s.gw != nil {
		// restic's REST protocol, for the host whose certificate the connection was made with (decision 041).
		mux.HandleFunc(gateway.Base, s.agentAuth(func(w http.ResponseWriter, r *http.Request) { s.gw.Serve(w, r, hostOf(r)) }))
	}
	if s.offsiteGW != nil {
		// A host's off-site copy, read-only, while a restore from off-site is granted to it (offsite.go).
		mux.HandleFunc(gateway.OffsiteBase, s.agentAuth(func(w http.ResponseWriter, r *http.Request) { s.offsiteGW.Serve(w, r, hostOf(r)) }))
	}
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

// AgentTLSConfig is the agent listener's TLS (decision 041). It asks every client for a certificate and verifies
// none against a CA: an agent's certificate is self-signed, and counts only because the server pinned it at
// enrollment. The TLS handshake proves the client holds its private key. /metrics and /healthz need no certificate.
func AgentTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequestClientCert}
}

// certificateOf returns the fingerprint of the certificate the request's connection was made with, or "".
func certificateOf(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	return pin.Fingerprint(r.TLS.PeerCertificates[0])
}

// agentAuth resolves the connection's certificate to the host that enrolled with it. The host identity always
// comes from here, never from a request body.
func (s *Server) agentAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fp := certificateOf(r)
		if fp == "" {
			writeErr(w, http.StatusUnauthorized, "no agent certificate: agents connect over TLS with the certificate they enrolled with")
			return
		}
		fqdn, err := s.st.Q().AgentByCertificate(r.Context(), fp)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unknown agent certificate: enroll this host (again) with holdfast-agent enroll")
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
	fp := certificateOf(r)
	if fp == "" {
		writeErr(w, http.StatusBadRequest, "enrollment needs the agent's own certificate on the connection: use holdfast-agent enroll, which makes it")
		return
	}
	if err := s.Enroll(r.Context(), req, fp); err != nil {
		// One message for every refusal; the audit log has the reason.
		writeErr(w, http.StatusForbidden, "enrollment refused: the code is invalid, expired or used, the host is unknown, "+
			"or its storage does not meet the contract; `holdfast audit --host <fqdn>` on the server says which")
		return
	}
	writeJSON(w, http.StatusOK, proto.EnrollResponse{CertificateSHA256: fp})
}

// Enroll redeems a code and pins the agent's certificate, by its fingerprint: from then on that certificate is the
// host (decision 041). The code is checked first. Storage is then set up again, idempotently, before the code is
// used, so a storage failure leaves the code valid for a retry.
func (s *Server) Enroll(ctx context.Context, req proto.EnrollRequest, certificate string) error {
	now := s.now()
	fqdn := strings.TrimSpace(req.FQDN)
	codeHash := creds.HashToken(creds.NormaliseCode(req.Code))
	refuse := func(reason error) error {
		_ = s.st.Q().Audit(ctx, store.AuditEntry{Actor: "agent:" + fqdn, Source: "agent", Action: "agent.enroll_refused", FQDN: fqdn,
			Detail: map[string]any{"reason": reason.Error(), "hostname_fqdn": req.Facts.HostnameFQDN}})
		return reason
	}
	if len(certificate) != 64 {
		return refuse(errors.New("no certificate fingerprint"))
	}
	h, err := s.st.Q().GetHost(ctx, fqdn)
	if err != nil {
		return refuse(store.ErrCodeInvalid)
	}
	if h.State == "retired" {
		return refuse(errors.New("host is retired"))
	}
	if err := s.checkProtocol(req.Facts); err != nil {
		return refuse(err)
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
	err = s.st.InTx(ctx, func(q *store.Q) error {
		if err := q.RedeemEnrollmentCode(ctx, fqdn, codeHash, now); err != nil {
			return err
		}
		if err := q.EnrollAgent(ctx, fqdn, certificate, now); err != nil {
			return err
		}
		detail := map[string]any{"agent_version": req.Facts.AgentVersion, "restic_version": req.Facts.ResticVersion,
			"os": req.Facts.OSID + " " + req.Facts.OSVersion, "hostname_fqdn": req.Facts.HostnameFQDN, "certificate_sha256": certificate}
		if req.Facts.HostnameFQDN != "" && req.Facts.HostnameFQDN != fqdn {
			detail["warning"] = "hostname -f does not match the FQDN" // plan-m1 Q9: a warning, not a refusal
		}
		return q.Audit(ctx, store.AuditEntry{Actor: "agent:" + fqdn, Source: "agent", Action: "agent.enrolled", FQDN: fqdn, Detail: detail})
	})
	if err != nil {
		return refuse(err)
	}
	return nil
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
		var old *AgentTooOldError
		if errors.As(err, &old) {
			writeTooOld(w, old)
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
		ClockOffsetMS: offset, AppliedRevision: req.Applied.Revision, AppliedSHA256: req.Applied.SHA256, CredentialsVersion: req.CredentialsVersion,
		RestoreBounds: req.Facts.RestoreBounds, Protocol: req.Facts.Protocol, Dumps: req.Facts.Dumps, Paused: req.Paused}, now); err != nil {
		return proto.PollResponse{}, err
	}
	if err := q.AckCommands(ctx, fqdn, req.AckedCommands, now); err != nil {
		return proto.PollResponse{}, err
	}
	// Seen, and its acknowledgements kept, but an agent too old gets nothing more (protocol.go).
	if err := s.checkProtocol(req.Facts); err != nil {
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
		if resp.Config == nil {
			// The agent runs the config it should. For a removed host that is the empty one: it is done.
			s.retireIfDone(ctx, fqdn)
		}
		remaining := time.Until(deadline)
		if changed || remaining <= 0 {
			resp.ServerTime = s.now()
			r := s.protocolRange()
			resp.Protocol = &r
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
		pw, err := s.seal.Open(c.RepoPassword, fqdn)
		if err != nil {
			return resp, false, err
		}
		resp.Credentials = &proto.Credentials{Version: c.Version, RepoPassword: pw}
		changed = true
	}
	cmds, err := q.PendingCommands(ctx, fqdn)
	if err != nil {
		return resp, false, err
	}
	for _, c := range cmds {
		if !acked[c.ID] {
			pc := proto.Command{ID: c.ID, Kind: c.Kind, JobID: c.JobID}
			if c.Kind == proto.CommandRestore && len(c.Payload) > 0 {
				pc.Restore = &proto.RestoreSpec{}
				if err := json.Unmarshal(c.Payload, pc.Restore); err != nil {
					return resp, false, fmt.Errorf("restore command %s: %w", c.ID, err)
				}
				// A restore from another repository: its password, and for a vault's copy its key, sealed for this host.
				if len(c.Sealed) > 0 {
					plain, err := s.seal.Open(c.Sealed, fqdn)
					if err != nil {
						return resp, false, fmt.Errorf("restore command %s: %w", c.ID, err)
					}
					pc.Restore.Source = &proto.RestoreSource{}
					if err := json.Unmarshal([]byte(plain), pc.Restore.Source); err != nil {
						return resp, false, fmt.Errorf("restore command %s: %w", c.ID, err)
					}
				}
			}
			if c.Kind == proto.CommandPause && len(c.Payload) > 0 {
				var until time.Time
				if err := json.Unmarshal(c.Payload, &until); err == nil {
					pc.Until = &until
				}
			}
			resp.Commands = append(resp.Commands, pc)
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
				TotalFiles: e.TotalFiles, TotalBytes: e.TotalBytes, ResticExit: e.ResticExit, Error: e.Error, Detail: e.Detail})
			if err != nil || e.Kind != proto.KindRestore || e.CommandID == "" {
				return err
			}
			// A restore from another host is over: so is the host's grant to read that one's repository.
			return q.EndGrant(ctx, e.CommandID, s.now())
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
