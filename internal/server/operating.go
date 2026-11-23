package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"filippo.io/age"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/restic"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/storage"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Operating Holdfast:
//   - escrow: each host's repository password, encrypted to an offline age key, in escrow/<fqdn>.age in the
//     bucket (decision 036);
//   - size: once a day, each host's prefix is listed with its versions;
//   - rotate: after a compromise, a new repository password, the agent's certificate unpinned and a new
//     enrollment code;
//   - decommission: a removed host's repository is deleted, 7 days after an admin asked for it.

const (
	escrowRetry         = 15 * time.Minute // a failed escrow write is tried again after this
	sizeEach            = 24 * time.Hour
	sizeRetry           = time.Hour
	decommissionRetry   = time.Hour
	DecommissionWait    = 7 * 24 * time.Hour // like every other deletion (CLAUDE.md)
	rotateLockWait      = 10 * time.Minute   // how long restic waits for a backup's lock when the old key is removed
	EscrowFormatVersion = "holdfast-escrow-1"
)

// operating is the state of the background work here.
type operating struct {
	escrowMu    sync.Mutex  // one escrow write at a time, so an older password never lands after a newer one
	measuring   atomic.Bool // a size listing is under way
	decommiting atomic.Bool // a decommission is under way
}

/* ---------------- escrow ---------------- */

// EscrowFile is what escrow/<fqdn>.age holds once decrypted with the offline key:
//
//	age -d -i key.txt web01.example.com.age
//
// The S3 key is not in it: the admin made it, and can make a new one (decision 036).
type EscrowFile struct {
	Format             string    `json:"format"`
	FQDN               string    `json:"fqdn"`
	Repository         string    `json:"repository"`
	RepoPassword       string    `json:"repo_password"`
	CredentialsVersion int       `json:"credentials_version"`
	WrittenAt          time.Time `json:"written_at"`
}

// escrowOn says whether server.yaml names an age recipient.
func (s *Server) escrowOn() bool { return s.set.Escrow.AgeRecipient != "" }

// writeEscrow writes a host's escrow file with its current password, and records the outcome. Without a
// recipient it does nothing. A failure is kept for the host's page, and tried again later.
func (s *Server) writeEscrow(ctx context.Context, fqdn string) error {
	if !s.escrowOn() {
		return nil
	}
	s.op.escrowMu.Lock()
	defer s.op.escrowMu.Unlock()
	q := s.st.Q()
	h, err := q.GetHost(ctx, fqdn)
	if err != nil {
		return err
	}
	c, err := q.GetCredentials(ctx, fqdn)
	if err != nil {
		return err
	}
	now := s.now()
	data, err := s.sealEscrow(c, now)
	if err == nil {
		err = s.bucket.PutEscrow(ctx, fqdn, data)
	}
	failure := ""
	if err != nil {
		failure = err.Error()
	}
	if rerr := q.RecordEscrow(ctx, fqdn, c.Version, now, failure); rerr != nil {
		return errors.Join(err, rerr)
	}
	switch {
	case err == nil:
		_ = q.Audit(ctx, store.AuditEntry{Actor: systemActor.Name, Source: systemActor.Source, Action: "escrow.written", FQDN: fqdn,
			Detail: map[string]any{"credentials_version": c.Version, "object": storage.EscrowKey(fqdn)}})
	case h.Escrow.Error == "": // the first failure is audited; the host's page shows the rest
		s.log.Warn("writing the escrow file failed", "host", fqdn, "err", err)
		_ = q.Audit(ctx, store.AuditEntry{Actor: systemActor.Name, Source: systemActor.Source, Action: "escrow.write_failed", FQDN: fqdn,
			Detail: map[string]any{"credentials_version": c.Version, "error": failure}})
	}
	return err
}

// sealEscrow encrypts a host's escrow file to the recipient. The server can encrypt, never decrypt.
func (s *Server) sealEscrow(c store.SealedCredentials, now time.Time) ([]byte, error) {
	rcpt, err := age.ParseX25519Recipient(s.set.Escrow.AgeRecipient)
	if err != nil {
		return nil, err
	}
	pw, err := s.seal.Open(c.RepoPassword, c.FQDN)
	if err != nil {
		return nil, err
	}
	plain, err := json.MarshalIndent(EscrowFile{Format: EscrowFormatVersion, FQDN: c.FQDN, Repository: c.Repository, RepoPassword: pw,
		CredentialsVersion: c.Version, WrittenAt: now.UTC()}, "", "  ")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, rcpt)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(append(plain, '\n')); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// escrowDue writes the escrow file of every host whose file does not hold its current password: a new host
// whose first write failed, a rotated password, or every host when a recipient is first set.
func (s *Server) escrowDue(ctx context.Context) {
	if !s.escrowOn() {
		return
	}
	hosts, err := s.st.Q().ListHosts(ctx)
	if err != nil {
		return
	}
	now := s.now()
	for _, h := range hosts {
		if h.Escrow.Current || decommissioned(h) || (h.Escrow.TriedAt != nil && now.Sub(*h.Escrow.TriedAt) < escrowRetry) {
			continue
		}
		_ = s.writeEscrow(ctx, h.FQDN)
	}
}

func decommissioned(h store.Host) bool { return h.Decommission != nil && h.Decommission.DoneAt != nil }

/* ---------------- size ---------------- */

// measureDue lists each host's prefix with its versions once a day, in the background.
func (s *Server) measureDue(ctx context.Context) {
	if !s.op.measuring.CompareAndSwap(false, true) {
		return
	}
	hosts, err := s.st.Q().ListHosts(ctx)
	if err != nil {
		s.op.measuring.Store(false)
		return
	}
	now := s.now()
	var due []string
	for _, h := range hosts {
		if decommissioned(h) {
			continue
		}
		wait := sizeEach - storageCheckSkew
		if h.SizeError != "" {
			wait = sizeRetry
		}
		if h.SizeTriedAt == nil || now.Sub(*h.SizeTriedAt) >= wait {
			due = append(due, h.FQDN)
		}
	}
	go func() {
		defer s.op.measuring.Store(false)
		for _, fqdn := range due {
			s.measure(ctx, fqdn)
		}
	}()
}

// measure lists one host's prefix and keeps the result.
func (s *Server) measure(ctx context.Context, fqdn string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	sz, err := s.bucket.Size(ctx, fqdn)
	failure := ""
	if err != nil {
		failure = err.Error()
		s.log.Warn("measuring the repository failed", "host", fqdn, "err", err)
	}
	if err := s.st.Q().RecordSize(ctx, fqdn, sz, s.now(), failure); err != nil {
		s.log.Error("recording the repository's size", "host", fqdn, "err", err)
	}
}

/* ---------------- rotate ---------------- */

// RotateInput is why the password is rotated, for the audit log.
type RotateInput struct {
	Message string `json:"message"`
}

// RotateResult is the outcome of RotateHost.
type RotateResult struct {
	CredentialsVersion int        `json:"credentials_version"`
	NewKey             string     `json:"new_key"`      // the repository key of the new password
	RemovedKeys        []string   `json:"removed_keys"` // the old password's key, and any left by an unfinished rotation
	Enrollment         Enrollment `json:"enrollment"`
	Escrowed           bool       `json:"escrowed"` // the escrow file holds the new password
	// Warning says what is not done yet, such as an old key that could not be removed.
	Warning string `json:"warning,omitempty"`
}

// RotateHost is what to do when a host is compromised:
//  1. a new repository password is added to the repository, and opens it;
//  2. the server stores it, and the agent's certificate is unpinned, so the host no longer gets anything;
//  3. the old password's key is removed from the repository;
//  4. the escrow file is written again, and a new enrollment code issued.
//
// The new password is stored before the old key is removed, so a crash in between never leaves the
// repository without a password the server has. An old key that could not be removed stays on record and is
// removed by the next rotation.
func (s *Server) RotateHost(ctx context.Context, a Actor, fqdn string, in RotateInput) (RotateResult, error) {
	if s.set.IsVault() {
		return RotateResult{}, fmt.Errorf("a compromised host is rotated on the server's pages; its off-site copies have a password of their own, which the host never had: %w", ErrConflict)
	}
	h, err := s.st.Q().GetHost(ctx, fqdn)
	if err != nil {
		return RotateResult{}, fmt.Errorf("host %s: %w", fqdn, err)
	}
	if h.State == "retired" {
		return RotateResult{}, fmt.Errorf("host %s is removed; add it again before rotating its password: %w", fqdn, ErrConflict)
	}
	if !s.maintBusy.acquire(fqdn) {
		return RotateResult{}, fmt.Errorf("a forget or prune is running on %s; try again when it is done: %w", fqdn, ErrConflict)
	}
	defer s.maintBusy.release(fqdn)

	old, cleanup, err := s.maintRunner(ctx, fqdn)
	if err != nil {
		return RotateResult{}, err
	}
	defer cleanup()
	old.RetryLock = rotateLockWait
	res := RotateResult{RemovedKeys: []string{}}
	// An earlier rotation's old key that could not be removed goes first, with the current password.
	if h.RepoKeyRetiring != "" {
		if err := s.removeOldKey(ctx, a, old, fqdn, h.RepoKeyRetiring); err != nil {
			return RotateResult{}, fmt.Errorf("an earlier rotation left key %s, which the old password opens, and it cannot be removed: %w", h.RepoKeyRetiring, err)
		}
		res.RemovedKeys = append(res.RemovedKeys, h.RepoKeyRetiring)
	}
	keys, err := old.Keys(ctx)
	if err != nil {
		return RotateResult{}, fmt.Errorf("listing the repository's keys: %w", err)
	}
	oldKey := ""
	for _, k := range keys {
		if k.Current {
			oldKey = k.ID
		}
	}
	if oldKey == "" {
		return RotateResult{}, errors.New("restic key list shows no key for the current password")
	}

	pw := creds.NewPassword()
	pf, pfCleanup, err := s.passwordFile(fqdn, pw)
	if err != nil {
		return RotateResult{}, err
	}
	defer pfCleanup()
	newKey, err := old.AddKey(ctx, pf)
	if err != nil {
		return RotateResult{}, fmt.Errorf("adding the new password: %w", err)
	}
	next := old
	next.PasswordFile = pf
	if nk, err := next.Keys(ctx); err != nil || !slices.ContainsFunc(nk, func(k restic.RepoKey) bool { return k.ID == newKey && k.Current }) {
		return RotateResult{}, fmt.Errorf("the new password does not open the repository (%v); nothing was changed, and key %s may be removed", err, newKey)
	}

	now := s.now()
	code := creds.NewEnrollmentCode()
	expires := now.Add(EnrollmentTTL)
	res.NewKey = newKey
	err = s.st.InTx(ctx, func(q *store.Q) error {
		c, err := q.GetCredentials(ctx, fqdn)
		if err != nil {
			return err
		}
		c.Version++
		c.RepoPassword, c.CreatedAt = s.seal.Seal(pw, fqdn), now
		if err := q.PutCredentials(ctx, c); err != nil {
			return err
		}
		res.CredentialsVersion = c.Version
		if err := q.StartRotation(ctx, fqdn, newKey, oldKey); err != nil {
			return err
		}
		if err := q.RevokeAgent(ctx, fqdn, "revoked:"+hex.EncodeToString(creds.Random(16))); err != nil {
			return err
		}
		if err := q.CreateEnrollmentCode(ctx, newID(), fqdn, creds.HashToken(creds.NormaliseCode(code)), now, expires, a.Name); err != nil {
			return err
		}
		if err := q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "host.rotated", FQDN: fqdn,
			Detail: map[string]any{"credentials_version": c.Version, "new_key": newKey, "old_key": oldKey, "agent_certificate": "unpinned", "message": in.Message}}); err != nil {
			return err
		}
		return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "enrollment.code_issued", FQDN: fqdn, Detail: map[string]any{"expires_at": expires}})
	})
	if err != nil {
		return RotateResult{}, fmt.Errorf("storing the new password (key %s was added to the repository and may be removed): %w", newKey, err)
	}
	res.Enrollment = s.enrollment(fqdn, code, expires)

	// The old key goes, with the new password: restic refuses to remove the key that opened the repository.
	var warnings []string
	if err := s.removeOldKey(ctx, a, next, fqdn, oldKey); err != nil {
		warnings = append(warnings, fmt.Sprintf("the old password still opens the repository: key %s could not be removed (%v); run the rotation again", oldKey, err))
	} else {
		res.RemovedKeys = append(res.RemovedKeys, oldKey)
	}
	if err := s.writeEscrow(ctx, fqdn); err != nil {
		warnings = append(warnings, "the escrow file could not be written: "+err.Error()+"; it is tried again")
	}
	if h2, err := s.st.Q().GetHost(ctx, fqdn); err == nil {
		res.Escrowed = h2.Escrow.Current
	}
	res.Warning = strings.Join(warnings, "; ")
	return res, nil
}

// removeOldKey removes a rotated-out key from the repository, unless it is gone already, and records that.
func (s *Server) removeOldKey(ctx context.Context, a Actor, r restic.Runner, fqdn, key string) error {
	keys, err := r.Keys(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(keys, func(k restic.RepoKey) bool { return k.ID == key }) {
		if err := r.RemoveKey(ctx, key); err != nil {
			return err
		}
	}
	return s.st.InTx(ctx, func(q *store.Q) error {
		if err := q.FinishRotation(ctx, fqdn, key); err != nil {
			return err
		}
		return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "repository.key_removed", FQDN: fqdn,
			Detail: map[string]any{"key": key, "reason": "its password was rotated"}})
	})
}

/* ---------------- decommission ---------------- */

// DecommissionInput confirms a decommission.
type DecommissionInput struct {
	Confirm string `json:"confirm"` // the host's FQDN, typed out
	Message string `json:"message"`
}

// Decommission asks for a removed host's repository to be deleted, after DecommissionWait. Only a retired
// host can be decommissioned, and only by an admin who types its FQDN.
func (s *Server) Decommission(ctx context.Context, a Actor, fqdn string, in DecommissionInput) (store.Decommission, error) {
	if strings.TrimSpace(in.Confirm) != fqdn {
		return store.Decommission{}, &ValidationError{config.Problems{{Path: "confirm", Message: "type the host's FQDN, " + fqdn + ", to confirm"}}}
	}
	h, err := s.st.Q().GetHost(ctx, fqdn)
	if err != nil {
		return store.Decommission{}, fmt.Errorf("host %s: %w", fqdn, err)
	}
	switch {
	case h.State != "retired":
		return store.Decommission{}, fmt.Errorf("only a removed host can be decommissioned; remove %s first (holdfast host remove): %w", fqdn, ErrConflict)
	case decommissioned(h):
		return store.Decommission{}, fmt.Errorf("host %s was decommissioned on %s: %w", fqdn, h.Decommission.DoneAt.Format(time.DateOnly), ErrConflict)
	case h.Decommission != nil:
		return store.Decommission{}, fmt.Errorf("the decommission of %s is waiting already, until %s: %w", fqdn, h.Decommission.DueAt.Format(time.DateTime), ErrConflict)
	}
	now := s.now()
	due := now.Add(DecommissionWait)
	err = s.st.InTx(ctx, func(q *store.Q) error {
		if err := q.RequestDecommission(ctx, fqdn, a.Name, now, due); err != nil {
			return err
		}
		return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "host.decommission_requested", FQDN: fqdn,
			Detail: map[string]any{"due_at": due, "message": in.Message}})
	})
	if err != nil {
		return store.Decommission{}, err
	}
	h, err = s.st.Q().GetHost(ctx, fqdn)
	if err != nil || h.Decommission == nil {
		return store.Decommission{}, err
	}
	return *h.Decommission, nil
}

// CancelDecommission cancels a decommission that is still waiting.
func (s *Server) CancelDecommission(ctx context.Context, a Actor, fqdn string, in RemoveInput) error {
	return s.st.InTx(ctx, func(q *store.Q) error {
		if err := q.CancelDecommission(ctx, fqdn); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return fmt.Errorf("no decommission of %s is waiting: %w", fqdn, ErrConflict)
			}
			return err
		}
		return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "host.decommission_cancelled", FQDN: fqdn,
			Detail: map[string]any{"message": in.Message}})
	})
}

// decommissionDue decommissions, in the background, each host whose wait is over.
func (s *Server) decommissionDue(ctx context.Context) {
	if !s.op.decommiting.CompareAndSwap(false, true) {
		return
	}
	hosts, err := s.st.Q().ListHosts(ctx)
	if err != nil {
		s.op.decommiting.Store(false)
		return
	}
	now := s.now()
	var due []store.Host
	for _, h := range hosts {
		d := h.Decommission
		if h.State == "retired" && d != nil && d.DoneAt == nil && !now.Before(d.DueAt) && (d.TriedAt == nil || now.Sub(*d.TriedAt) >= decommissionRetry) {
			due = append(due, h)
		}
	}
	go func() {
		defer s.op.decommiting.Store(false)
		for _, h := range due {
			s.decommission(ctx, h)
		}
	}()
}

// decommission deletes a host's repository and escrow file with the maintenance key. S3 keeps their old
// versions until the bucket's lifecycle rule expires them; that date is when the data is really gone.
func (s *Server) decommission(ctx context.Context, h store.Host) {
	if !s.maintBusy.acquire(h.FQDN) {
		return // a forget or prune; the next tick tries again
	}
	defer s.maintBusy.release(h.FQDN)
	q := s.st.Q()
	n, err := s.bucket.DeleteRepository(ctx, h.FQDN)
	now := s.now()
	if err != nil {
		s.log.Warn("decommission failed; it is tried again", "host", h.FQDN, "err", err)
		_ = q.RecordDecommissionFailure(ctx, h.FQDN, now, err.Error())
		if h.Decommission.Error == "" {
			_ = q.Audit(ctx, store.AuditEntry{Actor: systemActor.Name, Source: systemActor.Source, Action: "host.decommission_failed", FQDN: h.FQDN,
				Detail: map[string]any{"error": err.Error(), "objects_deleted": n}})
		}
		return
	}
	detail := map[string]any{"objects_deleted": n, "requested_by": h.Decommission.RequestedBy, "requested_at": h.Decommission.RequestedAt}
	if h.Storage != nil && h.Storage.NoncurrentDays > 0 {
		detail["really_gone_at"] = now.AddDate(0, 0, h.Storage.NoncurrentDays)
	}
	if err := s.st.InTx(ctx, func(q *store.Q) error {
		if err := q.MarkDecommissioned(ctx, h.FQDN, now, n); err != nil {
			return err
		}
		return q.Audit(ctx, store.AuditEntry{Actor: systemActor.Name, Source: systemActor.Source, Action: "host.decommissioned", FQDN: h.FQDN, Detail: detail})
	}); err != nil {
		s.log.Error("recording a decommission", "host", h.FQDN, "err", err)
	}
}

/* ---------------- API ---------------- */

func (s *Server) apiRotate(w http.ResponseWriter, r *http.Request) {
	var in RotateInput
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	res, err := s.RotateHost(r.Context(), actorOf(r), r.PathValue("fqdn"), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiDecommission(w http.ResponseWriter, r *http.Request) {
	var in DecommissionInput
	if !decode(w, r, &in) {
		return
	}
	d, err := s.Decommission(r.Context(), actorOf(r), r.PathValue("fqdn"), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) apiCancelDecommission(w http.ResponseWriter, r *http.Request) {
	var in RemoveInput
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	if err := s.CancelDecommission(r.Context(), actorOf(r), r.PathValue("fqdn"), in); err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}
