package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/kabsdk/holdfast/internal/access"
	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Me is who the asker is, and what they may do (plan-m4 §2). The UI builds its menus from it.
type Me struct {
	access.Principal
	// AdminEverywhere: admin on every host. Only they see the bindings, the sessions, and what belongs to no
	// host.
	AdminEverywhere bool `json:"admin_everywhere"`
	// AddHosts: some binding lets them add hosts (config.storage), on the FQDNs its patterns cover.
	AddHosts bool `json:"add_hosts"`
	// Hosts are the hosts they can see, with their permissions on each.
	Hosts map[string][]string `json:"hosts"`
}

// WhoAmI answers GET /api/v1/me.
func (s *Server) WhoAmI(ctx context.Context, a Actor) (Me, error) {
	v, err := s.viewerOf(ctx, a)
	if err != nil {
		return Me{}, err
	}
	m := Me{Principal: a, AdminEverywhere: v.everywhere(), Hosts: map[string][]string{}}
	if m.Groups == nil {
		m.Groups = []string{}
	}
	m.AddHosts = a.Everywhere
	for _, b := range v.b {
		if slices.Contains(a.Groups, b.Group) && slices.Contains(access.Roles[b.Role], access.ConfigStorage) {
			m.AddHosts = true
		}
	}
	m.AddHosts = m.AddHosts && !s.set.IsVault() // a vault's hosts come from its source
	hosts, err := s.st.Q().ListHosts(ctx)
	if err != nil {
		return Me{}, err
	}
	for _, h := range hosts {
		if perms := v.actions(h.FQDN); len(perms) > 0 {
			m.Hosts[h.FQDN] = perms
		}
	}
	return m, nil
}

// AccessView is the bindings in force (spec §6.3), the revision they are in, and what each role may do.
type AccessView struct {
	Revision int64               `json:"revision"`
	Bindings []config.Binding    `json:"bindings"`
	Roles    map[string][]string `json:"roles"`
}

// Access answers GET /api/v1/access, for admins everywhere: the bindings name every group and host pattern.
func (s *Server) Access(ctx context.Context, a Actor) (AccessView, error) {
	if err := s.requireEverywhere(ctx, a, access.ConfigAccess); err != nil {
		return AccessView{}, err
	}
	head, err := s.head(ctx, s.st.Q())
	if err != nil {
		return AccessView{}, err
	}
	return AccessView{Revision: head.Number, Bindings: head.Config.Access.Bindings, Roles: access.Roles}, nil
}

// SetAccessInput replaces the bindings.
type SetAccessInput struct {
	Base     *int64           `json:"base_revision,omitempty"`
	Message  string           `json:"message"`
	Bindings []config.Binding `json:"bindings"`
}

// SetAccess saves a revision with new bindings. Only an admin everywhere may: bindings decide who sees and
// does what on every host. A change that would leave nobody admin everywhere is refused, unless
// auth.bootstrap_admins or root on the server can still get in.
func (s *Server) SetAccess(ctx context.Context, a Actor, in SetAccessInput) (int64, error) {
	if err := s.requireEverywhere(ctx, a, access.ConfigAccess); err != nil {
		return 0, err
	}
	if strings.TrimSpace(in.Message) == "" {
		in.Message = "Change the role bindings"
	}
	if in.Bindings == nil {
		in.Bindings = []config.Binding{}
	}
	if s.set.IsVault() { // the server's bindings, from the feed (decision 041)
		return 0, fmt.Errorf("the off-site pages use the server's bindings: set them there, not under offsite: %w", ErrConflict)
	}
	var revNum int64
	err := s.st.InTx(ctx, func(q *store.Q) error {
		head, err := s.head(ctx, q)
		if err != nil {
			return err
		}
		if in.Base != nil && *in.Base != head.Number {
			return ErrConflict
		}
		doc := head.Config.Clone()
		doc.Access.Bindings = in.Bindings
		if p := doc.Validate(); len(p) > 0 {
			return &ValidationError{p}
		}
		if !slices.ContainsFunc(in.Bindings, func(b config.Binding) bool { return b.Role == "admin" && slices.Contains(b.Hosts, "*") }) &&
			len(s.set.Auth.BootstrapAdmins) == 0 {
			return &ValidationError{config.Problems{{Path: "bindings", Message: "no binding would make anyone admin on every host, and auth.bootstrap_admins is empty: " +
				"only root on the server could change the bindings again"}}}
		}
		rev, err := q.SaveRevision(ctx, store.SaveRevision{Base: head.Number, Actor: a.Name, Source: a.Source, Message: in.Message, Config: doc, HostSHAs: s.hostSHAs(doc)})
		if err != nil {
			return err
		}
		revNum = rev.Number
		return q.Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "access.change", Revision: rev.Number,
			Detail: map[string]any{"message": in.Message, "bindings": len(in.Bindings)}})
	})
	return revNum, err
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	m, err := s.WhoAmI(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) apiAccess(w http.ResponseWriter, r *http.Request) {
	v, err := s.Access(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) apiSetAccess(w http.ResponseWriter, r *http.Request) {
	var in SetAccessInput
	if !decode(w, r, &in) {
		return
	}
	rev, err := s.SetAccess(r.Context(), actorOf(r), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": rev})
}
