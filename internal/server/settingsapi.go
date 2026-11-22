package server

import (
	"context"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kabsdk/holdfast/internal/access"
	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/restic"
)

// SettingsView is what server.yaml says, without secrets (plan-m4 §5.2). Changing it needs root on the
// server (spec §3.3).
type SettingsView struct {
	// Role is "server", or "vault" for holdfast-vault, whose UI is red and shows the vault's actions (decision 039).
	Role          string `json:"role"`
	Source        string `json:"source,omitempty"` // a vault's source server
	Node          string `json:"node"`
	DefaultZone   string `json:"default_zone"`
	AgentURL      string `json:"agent_url"`
	UIURL         string `json:"ui_url,omitempty"`
	ResticVersion string `json:"restic_version"` // the one Holdfast is tested with, which the packages ship
	Storage       struct {
		Endpoint string `json:"endpoint"`
		Bucket   string `json:"bucket"`
		Region   string `json:"region,omitempty"`
	} `json:"storage"`
	Maintenance struct {
		Check          string `json:"check"`
		CheckCycleDays int    `json:"check_cycle_days"`
		Retention      string `json:"retention"`
	} `json:"maintenance"`
	Login struct {
		Issuer        string `json:"issuer,omitempty"`
		GroupsClaim   string `json:"groups_claim"`
		UsernameClaim string `json:"username_claim"`
		// BootstrapAdmins are shown to admins everywhere only.
		BootstrapAdmins []string `json:"bootstrap_admins,omitempty"`
	} `json:"login"`
	DefaultJob config.Job          `json:"default_job"` // where a new job starts
	Roles      map[string][]string `json:"roles"`
}

// SettingsFor answers GET /api/v1/settings.
func (s *Server) SettingsFor(ctx context.Context, a Actor) (SettingsView, error) {
	v, err := s.viewerOf(ctx, a)
	if err != nil {
		return SettingsView{}, err
	}
	var out SettingsView
	out.Role = s.set.Role
	if s.set.IsVault() {
		out.Source = s.set.Source.Server
	}
	out.Node, out.DefaultZone, out.AgentURL, out.UIURL, out.ResticVersion = s.set.NodeName, s.set.DefaultZone, s.set.PublicURL, s.set.UI.PublicURL, restic.PinnedVersion
	out.Storage.Endpoint, out.Storage.Bucket, out.Storage.Region = s.set.Storage.Endpoint, s.set.Storage.Bucket, s.set.Storage.Region
	m := s.set.Maintenance
	out.Maintenance.Check, out.Maintenance.CheckCycleDays, out.Maintenance.Retention = m.Check, m.CheckCycleDays, m.Retention
	o := s.set.Auth.OIDC
	out.Login.Issuer, out.Login.GroupsClaim, out.Login.UsernameClaim = o.Issuer, o.GroupsClaim, o.UsernameClaim
	if v.everywhere() {
		out.Login.BootstrapAdmins = slices.Clone(s.set.Auth.BootstrapAdmins)
	}
	out.DefaultJob, out.Roles = config.DefaultJob(), access.Roles
	return out, nil
}

// RetentionCutView is one retention cut that waits out the 7 days (plan-m4 Q11). It comes from the
// revisions alone: a change that accepted R104. The host's pending view has the snapshots and their counts.
type RetentionCutView struct {
	FQDN         string           `json:"fqdn"`
	JobID        string           `json:"job_id"`
	From         config.Retention `json:"from"`
	To           config.Retention `json:"to"`
	Revision     int64            `json:"revision"`
	ChangedAt    time.Time        `json:"changed_at"`
	ChangedBy    string           `json:"changed_by"`
	PrunableFrom time.Time        `json:"prunable_from"`
}

// RetentionCuts lists the cuts made during the last 7 days, on the actor's hosts, newest first.
func (s *Server) RetentionCuts(ctx context.Context, a Actor) ([]RetentionCutView, error) {
	v, err := s.viewerOf(ctx, a)
	if err != nil {
		return nil, err
	}
	revs, err := s.st.Q().RevisionsInForce(ctx, s.now().Add(-Protection))
	if err != nil {
		return nil, err
	}
	out := []RetentionCutView{}
	for i := 1; i < len(revs); i++ {
		prev, rev := revs[i-1], revs[i]
		if !slices.Contains(rev.Accepted, "R104") {
			continue
		}
		for fqdn, h := range rev.Config.Hosts {
			if !v.sees(fqdn) {
				continue
			}
			for id, j := range h.Jobs {
				old, ok := prev.Config.Hosts[fqdn].Jobs[id]
				if !ok || strings.Join(old.Retention.ForgetArgs(), " ") == strings.Join(j.Retention.ForgetArgs(), " ") {
					continue
				}
				out = append(out, RetentionCutView{FQDN: fqdn, JobID: id, From: old.Retention, To: j.Retention, Revision: rev.Number,
					ChangedAt: rev.CreatedAt, ChangedBy: rev.Actor, PrunableFrom: rev.CreatedAt.Add(Protection)})
			}
		}
	}
	sort.SliceStable(out, func(i, k int) bool { return out[i].Revision > out[k].Revision })
	return out, nil
}

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	v, err := s.SettingsFor(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) apiRetentionCuts(w http.ResponseWriter, r *http.Request) {
	list, err := s.RetentionCuts(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
