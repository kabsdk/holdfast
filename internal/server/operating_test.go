package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/kabsdk/holdfast/internal/server/storage"
)

// failingBucket fails every escrow write, as a maintenance key without escrow/* would.
type failingBucket struct{ storage.MemBucket }

func (*failingBucket) PutEscrow(context.Context, string, []byte) error {
	return errors.New("writing escrow/x.age: AccessDenied")
}

func hasProblem(v HostView, code string) bool {
	return slices.ContainsFunc(v.Problems, func(p Problem) bool { return p.Code == code })
}

// Decision 036: the repository password goes into the bucket at host add, encrypted to the offline key, and
// only that key opens it.
func TestEscrowAtHostAdd(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	b := &storage.MemBucket{}
	s, ctx := newTestServerWith(t, func(d *Deps) { d.Bucket = b }), context.Background()
	const fqdn = "web01.example.com"

	// Without a recipient nothing is written, and the host's page says so.
	if _, err := s.AddHost(ctx, root, AddHostInput{FQDN: "off01.example.com"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Host(ctx, "off01.example.com"); !hasProblem(v, "escrow") || v.Escrow.Current || len(b.Escrow) != 0 {
		t.Fatalf("without a recipient: %+v %+v", v.Problems, v.Escrow)
	}

	s.set.Escrow.AgeRecipient = id.Recipient().String()
	if _, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn, JobID: "system", Job: job("/etc")}); err != nil {
		t.Fatal(err)
	}
	r, err := age.Decrypt(bytes.NewReader(b.Escrow[fqdn]), id)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(r)
	var f EscrowFile
	if err := json.Unmarshal(plain, &f); err != nil {
		t.Fatal(err)
	}
	c, _ := s.st.Q().GetCredentials(ctx, fqdn)
	pw, _ := s.seal.Open(c.RepoPassword, fqdn)
	if f.Format != EscrowFormatVersion || f.FQDN != fqdn || f.RepoPassword != pw || f.Repository != c.Repository || f.CredentialsVersion != c.Version {
		t.Fatalf("escrow file: %+v", f)
	}
	if bytes.Contains(b.Escrow[fqdn], []byte(pw)) {
		t.Fatal("the escrow file must be encrypted")
	}
	v, _ := s.Host(ctx, fqdn)
	if !v.Escrow.Current || hasProblem(v, "escrow") {
		t.Fatalf("after the write: %+v %+v", v.Escrow, v.Problems)
	}

	// The host added before the recipient was set is written by the background pass.
	s.escrowDue(ctx)
	if v, _ := s.Host(ctx, "off01.example.com"); !v.Escrow.Current || len(b.Escrow) != 2 {
		t.Fatalf("backfill: %+v", v.Escrow)
	}
}

// A failed escrow write is shown, audited once, and tried again later.
func TestEscrowWriteFails(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	s, ctx := newTestServerWith(t, func(d *Deps) {
		d.Bucket = &failingBucket{}
		d.Now = func() time.Time { return now }
	}), context.Background()
	s.set.Escrow.AgeRecipient = id.Recipient().String()
	const fqdn = "web01.example.com"
	res, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn})
	if err != nil {
		t.Fatal(err)
	}
	if res.Warning == "" {
		t.Fatal("host add must say the escrow file was not written")
	}
	v, _ := s.Host(ctx, fqdn)
	if v.Escrow.Current || v.Escrow.Error == "" || !hasProblem(v, "escrow") {
		t.Fatalf("a failed write: %+v %+v", v.Escrow, v.Problems)
	}
	tried := *v.Escrow.TriedAt
	s.escrowDue(ctx) // too soon
	if h, _ := s.st.Q().GetHost(ctx, fqdn); !h.Escrow.TriedAt.Equal(tried) {
		t.Fatal("tried again too soon")
	}
	now = now.Add(escrowRetry)
	s.escrowDue(ctx)
	if h, _ := s.st.Q().GetHost(ctx, fqdn); h.Escrow.TriedAt.Equal(tried) {
		t.Fatal("not tried again")
	}
	audit, _ := s.st.Q().ListAudit(ctx, fqdn, 50)
	n := 0
	for _, e := range audit {
		if e.Action == "escrow.write_failed" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the first failure is audited, once: %d", n)
	}
}

// Plan-m5 item 7: only a removed host, only with its FQDN typed, only after 7 days, and cancellable.
func TestDecommission(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	b := &storage.MemBucket{}
	s, ctx := newTestServerWith(t, func(d *Deps) {
		d.Bucket = b
		d.Now = func() time.Time { return now }
	}), context.Background()
	const fqdn = "old01.example.com"
	if _, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn, JobID: "system", Job: job("/etc")}); err != nil {
		t.Fatal(err)
	}
	c1, _ := s.st.Q().GetCredentials(ctx, fqdn)

	if _, err := s.Decommission(ctx, root, fqdn, DecommissionInput{Confirm: fqdn}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a host still in the config: %v", err)
	}
	if _, err := s.RemoveHost(ctx, root, fqdn, RemoveInput{}); err != nil {
		t.Fatal(err)
	}
	var ve *ValidationError
	if _, err := s.Decommission(ctx, root, fqdn, DecommissionInput{Confirm: "old01"}); !errors.As(err, &ve) {
		t.Fatalf("the FQDN must be typed: %v", err)
	}
	d, err := s.Decommission(ctx, root, fqdn, DecommissionInput{Confirm: fqdn, Message: "rack 4 is gone"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.DueAt.Equal(now.Add(DecommissionWait)) || d.RequestedBy != root.Name {
		t.Fatalf("decommission: %+v", d)
	}
	if _, err := s.Decommission(ctx, root, fqdn, DecommissionInput{Confirm: fqdn}); !errors.Is(err, ErrConflict) {
		t.Fatalf("asked twice: %v", err)
	}
	if _, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn}); !errors.Is(err, ErrConflict) {
		t.Fatalf("added back while waiting: %v", err)
	}
	if v, _ := s.Host(ctx, fqdn); !hasProblem(v, "decommission") {
		t.Fatalf("a waiting decommission is shown: %+v", v.Problems)
	}

	// Cancelled, and asked for again.
	if err := s.CancelDecommission(ctx, root, fqdn, RemoveInput{}); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelDecommission(ctx, root, fqdn, RemoveInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("nothing to cancel: %v", err)
	}
	if _, err := s.Decommission(ctx, root, fqdn, DecommissionInput{Confirm: fqdn}); err != nil {
		t.Fatal(err)
	}

	// Nothing happens before the wait is over.
	runDecommissions := func() {
		s.decommissionDue(ctx)
		for s.op.decommiting.Load() {
			time.Sleep(10 * time.Millisecond)
		}
	}
	now = now.Add(DecommissionWait - time.Minute)
	runDecommissions()
	if b.Deleted[fqdn] {
		t.Fatal("deleted before the 7 days were over")
	}
	now = now.Add(time.Minute)
	runDecommissions()
	if !b.Deleted[fqdn] {
		t.Fatal("not deleted after the 7 days")
	}
	h, _ := s.st.Q().GetHost(ctx, fqdn)
	if h.Decommission == nil || h.Decommission.DoneAt == nil || h.State != "retired" {
		t.Fatalf("after: %+v", h.Decommission)
	}
	if err := s.CancelDecommission(ctx, root, fqdn, RemoveInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled after it ran: %v", err)
	}

	// Added again, it starts over: a new repository password.
	if _, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn}); err != nil {
		t.Fatal(err)
	}
	c2, _ := s.st.Q().GetCredentials(ctx, fqdn)
	pw1, _ := s.seal.Open(c1.RepoPassword, fqdn)
	pw2, _ := s.seal.Open(c2.RepoPassword, fqdn)
	if pw1 == pw2 {
		t.Fatal("a decommissioned host added again must get a new repository password")
	}
	if h, _ := s.st.Q().GetHost(ctx, fqdn); h.Decommission != nil {
		t.Fatalf("the decommission record must be cleared: %+v", h.Decommission)
	}
}

// The daily listing keeps the repository's size, and a failure is shown.
func TestRepositorySize(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	b := &storage.MemBucket{Sizes: map[string]storage.Size{"web01.example.com": {Current: 3 << 20, Noncurrent: 1 << 20, Objects: 7}}}
	s, ctx := newTestServerWith(t, func(d *Deps) {
		d.Bucket = b
		d.Now = func() time.Time { return now }
	}), context.Background()
	if _, err := s.AddHost(ctx, root, AddHostInput{FQDN: "web01.example.com"}); err != nil {
		t.Fatal(err)
	}
	s.measureDue(ctx)
	for s.op.measuring.Load() {
		time.Sleep(10 * time.Millisecond)
	}
	v, _ := s.Host(ctx, "web01.example.com")
	if v.Size == nil || v.Size.Current != 3<<20 || v.Size.Noncurrent != 1<<20 || v.Size.Objects != 7 || !v.Size.At.Equal(now) {
		t.Fatalf("size: %+v", v.Size)
	}
}
