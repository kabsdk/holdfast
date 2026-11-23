package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kabsdk/holdfast/internal/access"
	"github.com/kabsdk/holdfast/internal/config"
)

// apiAs calls the API handler as actor a and returns the status and body.
func apiAs(t *testing.T, h http.Handler, a Actor, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd).WithContext(WithActor(context.Background(), a))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestPermissions(t *testing.T) {
	s, ctx := newTestServer(t), context.Background()
	for _, fqdn := range []string{"web01.example.com", "db01.example.com"} {
		if _, err := s.AddHost(ctx, root, AddHostInput{FQDN: fqdn, JobID: "system", Job: job("/etc")}); err != nil {
			t.Fatal(err)
		}
	}
	bindings := append(config.DefaultAccess().Bindings, config.Binding{Group: "web-team", Role: "viewer", Hosts: []string{"web01.example.com"}})
	if _, err := s.SetAccess(ctx, root, SetAccessInput{Bindings: bindings}); err != nil {
		t.Fatal(err)
	}
	h := s.APIHandler(false)
	admin := Actor{Name: "ada", Source: "ui", Groups: []string{"holdfast-admins"}}
	editor := Actor{Name: "ed", Source: "ui", Groups: []string{"holdfast-editors"}}
	viewer := Actor{Name: "vi", Source: "ui", Groups: []string{"holdfast-viewers"}}
	web := Actor{Name: "wes", Source: "cli", Groups: []string{"web-team"}}
	nobody := Actor{Name: "nob", Source: "ui"}

	// The host list, and the actions on each host.
	for _, tc := range []struct {
		a    Actor
		want []string
	}{{admin, []string{"db01.example.com", "web01.example.com"}}, {web, []string{"web01.example.com"}}, {nobody, nil}} {
		_, body := apiAs(t, h, tc.a, "GET", "/api/v1/hosts", nil)
		var hosts []HostView
		_ = json.Unmarshal(body, &hosts)
		var got []string
		for _, x := range hosts {
			got = append(got, x.FQDN)
			if !slices.Equal(x.Actions, tc.a.On(x.FQDN, bindings)) {
				t.Errorf("%s on %s: actions %v", tc.a.Name, x.FQDN, x.Actions)
			}
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s sees %v, want %v", tc.a.Name, got, tc.want)
		}
	}

	for _, tc := range []struct {
		name   string
		a      Actor
		method string
		path   string
		body   any
		want   int
	}{
		{"a host you cannot see does not exist", web, "GET", "/api/v1/hosts/db01.example.com", nil, 404},
		{"nor its snapshots", web, "GET", "/api/v1/hosts/db01.example.com/snapshots", nil, 404},
		{"a host you can see", web, "GET", "/api/v1/hosts/web01.example.com", nil, 200},
		{"a viewer sees no file names", viewer, "GET", "/api/v1/hosts/web01.example.com/find?pattern=passwd", nil, 403},
		{"a viewer cannot back up", viewer, "POST", "/api/v1/hosts/web01.example.com/jobs/system/run", nil, 403},
		{"an editor can", editor, "POST", "/api/v1/hosts/web01.example.com/jobs/system/run", nil, 202},
		{"an editor cannot restore in place", editor, "POST", "/api/v1/hosts/web01.example.com/snapshots/aaaaaaaa/restore", map[string]any{"target": "in_place"}, 403},
		{"an editor cannot add a host", editor, "POST", "/api/v1/hosts", map[string]any{"fqdn": "new01.example.com"}, 403},
		{"an editor cannot Prune now", editor, "GET", "/api/v1/hosts/web01.example.com/prune-now", nil, 403},
		{"an editor cannot delete a snapshot", editor, "POST", "/api/v1/hosts/web01.example.com/snapshots/aaaaaaaa/delete", map[string]any{}, 403},
		{"an editor cannot acknowledge", editor, "POST", "/api/v1/hosts/web01.example.com/acknowledge", nil, 403},
		{"an editor cannot rotate the password", editor, "POST", "/api/v1/hosts/web01.example.com/rotate", nil, 403},
		{"an editor cannot decommission", editor, "POST", "/api/v1/hosts/web01.example.com/decommission", map[string]any{"confirm": "web01.example.com"}, 403},
		{"an admin cannot decommission a host still in the config", admin, "POST", "/api/v1/hosts/web01.example.com/decommission", map[string]any{"confirm": "web01.example.com"}, 409},
		{"an editor cannot see the bindings", editor, "GET", "/api/v1/access", nil, 403},
		{"an admin can", admin, "GET", "/api/v1/access", nil, 200},
		{"nor the sessions", editor, "GET", "/api/v1/sessions", nil, 403},
		{"ui-login is not on this listener", admin, "POST", "/api/v1/ui-login", nil, 404},
	} {
		if code, body := apiAs(t, h, tc.a, tc.method, tc.path, tc.body); code != tc.want {
			t.Errorf("%s: %s %s as %s: %d, want %d: %s", tc.name, tc.method, tc.path, tc.a.Name, code, tc.want, body)
		}
	}

	// Revisions and the audit log are cut down to your hosts.
	var revs []struct{ Number int64 }
	_, body := apiAs(t, h, web, "GET", "/api/v1/revisions", nil)
	_ = json.Unmarshal(body, &revs)
	if len(revs) != 1 || revs[0].Number != 1 {
		t.Fatalf("web-team sees only the revision that added web01: %s", body)
	}
	if code, _ := apiAs(t, h, web, "GET", "/api/v1/revisions/2", nil); code != 404 {
		t.Fatalf("the revision that added db01 does not exist for web-team: %d", code)
	}
	_, body = apiAs(t, h, web, "GET", "/api/v1/revisions/1", nil)
	if strings.Contains(string(body), "db01") || strings.Contains(string(body), "holdfast-admins") {
		t.Fatalf("a revision shows only your hosts, and no bindings: %s", body)
	}
	_, body = apiAs(t, h, web, "GET", "/api/v1/revisions/1/diff/3", nil)
	if strings.Contains(string(body), "db01") || strings.Contains(string(body), "access") {
		t.Fatalf("a diff shows only your hosts: %s", body)
	}
	_, body = apiAs(t, h, web, "GET", "/api/v1/audit", nil)
	var audit []struct{ Action, FQDN string }
	_ = json.Unmarshal(body, &audit)
	for _, e := range audit {
		if e.FQDN != "web01.example.com" {
			t.Fatalf("web-team sees an audit entry that is not about web01: %+v", e)
		}
	}
	_, body = apiAs(t, h, admin, "GET", "/api/v1/audit?action=access.denied", nil)
	if !strings.Contains(string(body), `"permission":"`+access.Browse) {
		t.Fatalf("refusals are audited: %s", body)
	}

	// Bindings that leave nobody admin everywhere are refused.
	if code, _ := apiAs(t, h, admin, "PUT", "/api/v1/access", SetAccessInput{Bindings: []config.Binding{{Group: "x", Role: "viewer", Hosts: []string{"*"}}}}); code != 422 {
		t.Fatalf("locking everyone out: %d", code)
	}
	// What you may do, for the UI's menus.
	_, body = apiAs(t, h, web, "GET", "/api/v1/me", nil)
	var me Me
	_ = json.Unmarshal(body, &me)
	if me.AdminEverywhere || me.AddHosts || len(me.Hosts) != 1 || !slices.Equal(me.Hosts["web01.example.com"], []string{access.View}) {
		t.Fatalf("me: %s", body)
	}
}
