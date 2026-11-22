package access

import (
	"slices"
	"testing"

	"github.com/kabsdk/holdfast/internal/config"
)

var bindings = append(config.DefaultAccess().Bindings,
	config.Binding{Group: "web-team", Role: "editor", Hosts: []string{"web*.example.com"}},
	config.Binding{Group: "web-team", Role: "viewer", Hosts: []string{"db01.example.com"}},
	config.Binding{Group: "dc1-admins", Role: "admin", Hosts: []string{"*.dc1.example.com"}},
)

func TestRolesAreFlat(t *testing.T) {
	if !slices.Equal(Roles["viewer"], []string{View}) {
		t.Fatalf("viewer: %v", Roles["viewer"])
	}
	if slices.Contains(Roles["editor"], RestoreInPlace) || slices.Contains(Roles["editor"], RetentionCut) || slices.Contains(Roles["editor"], ConfigStorage) {
		t.Fatalf("an editor does nothing destructive: %v", Roles["editor"])
	}
	if !slices.Contains(Roles["editor"], Browse) || slices.Contains(Roles["viewer"], Browse) {
		t.Fatal("editors browse file names, viewers do not (decision 033)")
	}
}

func TestOn(t *testing.T) {
	web := Principal{Name: "anna", Groups: []string{"web-team"}}
	for _, tc := range []struct {
		p    Principal
		fqdn string
		want []string
	}{
		{web, "web01.example.com", Roles["editor"]},
		{web, "db01.example.com", []string{View}},
		{web, "mail01.example.com", []string{}}, // no binding: the host does not exist for anna
		{Principal{Groups: []string{"web-team", "dc1-admins"}}, "web01.dc1.example.com", All},
		{Principal{Groups: []string{"holdfast-viewers"}}, "anything.example.com", []string{View}},
		{Principal{Groups: []string{"holdfast-viewers", "web-team"}}, "web02.example.com", Roles["editor"]}, // bindings combine
		{Principal{Name: "local:root", Everywhere: true}, "mail01.example.com", All},
		{Principal{Groups: nil}, "web01.example.com", []string{}},
	} {
		if got := tc.p.On(tc.fqdn, bindings); !slices.Equal(got, tc.want) {
			t.Errorf("%v on %s: got %v, want %v", tc.p.Groups, tc.fqdn, got, tc.want)
		}
	}
	if web.Sees("mail01.example.com", bindings) || !web.Sees("db01.example.com", bindings) {
		t.Fatal("Sees")
	}
	if web.Can(Browse, "db01.example.com", bindings) || !web.Can(Browse, "web01.example.com", bindings) {
		t.Fatal("Can")
	}
}

func TestAdminEverywhere(t *testing.T) {
	if !(Principal{Groups: []string{"holdfast-admins"}}).AdminEverywhere(bindings) {
		t.Fatal("an admin on * is admin everywhere")
	}
	if (Principal{Groups: []string{"dc1-admins"}}).AdminEverywhere(bindings) {
		t.Fatal("an admin on some hosts is not")
	}
	if !(Principal{Everywhere: true}).AdminEverywhere(nil) {
		t.Fatal("root is")
	}
}

func TestPatterns(t *testing.T) {
	for p, fqdn := range map[string]string{"*": "a.b", "web*.example.com": "web01.example.com", "*.dc1.example.com": "x.dc1.example.com"} {
		if !Matches(p, fqdn) || ValidPattern(p) != nil {
			t.Errorf("%s should match %s", p, fqdn)
		}
	}
	if Matches("*.dc1.example.com", "dc1.example.com") || Matches("web*", "db01") {
		t.Fatal("too broad")
	}
	for _, bad := range []string{"", "WEB*", "web[0-9]", "a/b", "a b"} {
		if ValidPattern(bad) == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}
