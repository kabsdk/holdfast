package server

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/gateway"
	"github.com/kabsdk/holdfast/internal/server/creds"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Off-site copies (decision 041, plan-m8 part 4). With offsite: in server.yaml, holdfast-server runs a second instance
// of its own code beside itself: the off-site instance, in the vault role, on the off-site bucket, with its tables in
// the schema "offsite" of the same database. It is M7's vault, without a program, a token or keys of its own:
//
//   - it copies each job's new snapshots with restic copy, reading the primary repository through a loopback endpoint
//     that serves it read-only (copierSource), and writing the off-site one with the off-site key;
//   - it keeps them under each job's off-site retention, behind the same 7-day protection, with its own Prune now,
//     Delete now and "Delete here too", its own config history and audit log;
//   - the two talk in-process: every poll it hands over its report and takes the feed (VaultSync);
//   - people reach it through the server's UI, API and CLI, under /api/v1/offsite/ (serveOffsite), with the same
//     login and the same bindings, which the feed carries;
//   - a restore from off-site runs on the host, through the gateway's off-site endpoint, read-only (offsiteAccess).
//
// Without offsite:, none of this exists: s.offsite is nil, and nothing about off-site is shown or checked.

// offsiteActor is the off-site instance's own work, as holdfast-server's is systemActor.
var offsiteActor = Actor{Name: "holdfast-server:offsite", Source: "system"}

// AttachOffsite links the off-site instance to this server. Call it before Run and before AgentHandler, APIHandler
// and UIHandler: they then serve the off-site endpoints too. ctx ends the copier's loopback endpoint.
func (s *Server) AttachOffsite(ctx context.Context, off *Server) error {
	if s.set.IsVault() || !off.set.IsVault() {
		return errors.New("AttachOffsite links a server to its off-site instance")
	}
	src, err := s.startCopierSource(ctx)
	if err != nil {
		return err
	}
	gw, err := gateway.New(gateway.Options{Base: gateway.OffsiteBase, Endpoint: off.loc.Endpoint, Bucket: off.loc.Bucket, Region: off.loc.Region,
		AccessKeyID: off.maintKey.AccessKeyID, SecretAccessKey: off.maintKey.SecretAccessKey,
		Access: s.offsiteAccess, Refused: s.gatewayRefused, Log: s.log})
	if err != nil {
		return err
	}
	if err := s.st.Q().EnsureVault(ctx, store.OffsiteName, systemActor.Name, s.now()); err != nil {
		return err
	}
	s.offsite, s.offsiteGW, s.copierSource = off, gw, src
	s.offsiteAPI = [2]http.Handler{off.APIHandler(false), off.APIHandler(true)}
	off.source = s
	return nil
}

// Offsite says whether off-site copies are set up.
func (s *Server) Offsite() bool { return s.offsite != nil }

// startCopierSource serves the primary repositories, read-only, on a loopback port, with a secret of its own: the
// copier's restic reads them there, so it needs no key for the primary bucket, and writes off-site with the off-site
// key alone. restic takes one set of S3 credentials for both repositories of a copy, which is why.
func (s *Server) startCopierSource(ctx context.Context) (string, error) {
	gw, err := gateway.New(gateway.Options{Endpoint: s.loc.Endpoint, Bucket: s.loc.Bucket, Region: s.loc.Region,
		AccessKeyID: s.gwKey.AccessKeyID, SecretAccessKey: s.gwKey.SecretAccessKey,
		Access: func(context.Context, string, string) gateway.Access { return gateway.ReadOnly },
		Refused: func(_ context.Context, _ string, r gateway.Request, ref *gateway.Refusal) {
			s.log.Error("the copier's source refused a request", "repository", r.Repo, "op", r.Op, "kind", r.Kind, "reason", ref.Reason)
		}, Log: s.log})
	if err != nil {
		return "", err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("the copier's loopback endpoint: %w", err)
	}
	secret := hex.EncodeToString(creds.Random(24))
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "copier" || subtle.ConstantTimeCompare([]byte(p), []byte(secret)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		gw.Serve(w, r, "copier")
	})}
	go func() { _ = srv.Serve(l) }()
	go func() { <-ctx.Done(); _ = srv.Close() }()
	return "http://copier:" + secret + "@" + l.Addr().String() + gateway.Base, nil
}

// offsiteAccess lets a host read its own off-site copy through the gateway while a restore from off-site is granted to
// it, and nothing else: hosts never write off-site.
func (s *Server) offsiteAccess(ctx context.Context, host, repo string) gateway.Access {
	if host != repo {
		return gateway.NoAccess
	}
	if ok, err := s.st.Q().HasGrant(ctx, host, repo, store.SiteOffsite, s.now()); err == nil && ok {
		return gateway.ReadOnly
	}
	return gateway.NoAccess
}

// serveOffsite answers /api/v1/offsite/...: the off-site instance's own API, as /api/v1/..., for the actor this server
// authenticated. Its bindings are this server's, from the feed.
func (s *Server) serveOffsite(local bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.offsite == nil {
			writeErr(w, http.StatusNotFound, "off-site copies are not set up: they need an offsite: section in server.yaml (docs/storage.md)")
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/api/v1" + strings.TrimPrefix(r.URL.Path, "/api/v1/offsite")
		r2.URL.RawPath = ""
		h := s.offsiteAPI[0]
		if local {
			h = s.offsiteAPI[1]
		}
		h.ServeHTTP(w, r2)
	}
}

// writeOffsiteMetrics writes the off-site instance's own metrics into the server's: the lag, a failing copy, and its
// storage contract, by host. The alert rules compare the lag with the job's longest gap, which the server exports.
func (s *Server) writeOffsiteMetrics(ctx context.Context, w io.Writer) {
	off := s.offsite
	if off == nil {
		return
	}
	hosts, err := off.Hosts(ctx)
	if err != nil {
		s.log.Warn("the off-site metrics", "err", err)
		return
	}
	sort.Slice(hosts, func(i, k int) bool { return hosts[i].FQDN < hosts[k].FQDN })
	gauge := func(name, help string) { fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name) }
	gauge("holdfast_offsite_behind_seconds", "How far the newest off-site copy of the job is behind its newest snapshot in primary (decision 039).")
	for _, h := range hosts {
		for _, j := range h.Jobs {
			if j.BehindS != nil && !j.Excluded && !j.Retired {
				fmt.Fprintf(w, "holdfast_offsite_behind_seconds{host=%q,job_id=%q} %d\n", h.FQDN, j.ID, *j.BehindS)
			}
		}
	}
	gauge("holdfast_offsite_copy_failed", "1 if the host's last copy to off-site failed.")
	for _, h := range hosts {
		failed := 0
		if h.CopyError != "" {
			failed = 1
		}
		fmt.Fprintf(w, "holdfast_offsite_copy_failed{host=%q} %d\n", h.FQDN, failed)
	}
	gauge("holdfast_offsite_storage_contract_ok", "1 if the off-site bucket passed every storage check at the host's last test.")
	for _, h := range hosts {
		if h.Storage == nil {
			continue
		}
		ok := 1
		if !h.Storage.OK() {
			ok = 0
		}
		fmt.Fprintf(w, "holdfast_offsite_storage_contract_ok{host=%q} %d\n", h.FQDN, ok)
	}
}
