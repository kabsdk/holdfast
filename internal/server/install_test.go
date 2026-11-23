package server

import (
	"strings"
	"testing"
)

// The one-line install that host add prints (decision 044): the release of this server, or the mirror in download_url.
func TestInstallScript(t *testing.T) {
	pin := strings.Repeat("ab", 32)
	s := &Server{set: Settings{PublicURL: "https://holdfast.example.com:8443", CASHA256: pin}}
	defer func(v string) { Version = v }(Version)

	Version = "1.2.0"
	got := s.installScript("web01.example.com", "ABCD-EFGH")
	want := "curl -sfL " + Releases + "/download/v1.2.0/install.sh | HOLDFAST_SERVER=https://holdfast.example.com:8443 " +
		"HOLDFAST_FQDN=web01.example.com HOLDFAST_CODE=ABCD-EFGH HOLDFAST_CA_SHA256=" + pin + " HOLDFAST_VERSION=1.2.0 sh -"
	if got != want {
		t.Fatalf("release:\n got %s\nwant %s", got, want)
	}

	// A build that is not a release: the latest release.
	Version = "dev"
	if got := s.installScript("web01.example.com", "C"); !strings.HasPrefix(got, "curl -sfL "+Releases+"/latest/download/install.sh | ") ||
		strings.Contains(got, "HOLDFAST_VERSION") {
		t.Fatalf("dev build: %s", got)
	}

	// A mirror: install.sh and the packages from there, for any version.
	s.set.DownloadURL = "https://mirror.example.com/holdfast/1.2.0/"
	got = s.installScript("web01.example.com", "C")
	if !strings.HasPrefix(got, "curl -sfL https://mirror.example.com/holdfast/1.2.0/install.sh | ") ||
		!strings.Contains(got, " HOLDFAST_DOWNLOAD_URL=https://mirror.example.com/holdfast/1.2.0 sh -") {
		t.Fatalf("mirror: %s", got)
	}

	// Anything a shell would read differently is quoted.
	s.set.PublicURL = "https://holdfast.example.com:8443/a b'c"
	if got := s.installScript("web01.example.com", "C"); !strings.Contains(got, `HOLDFAST_SERVER='https://holdfast.example.com:8443/a b'\''c' `) {
		t.Fatalf("quoting: %s", got)
	}
}

func TestDownloadURLSetting(t *testing.T) {
	for _, u := range []string{"ftp://mirror/x", "https://mirror/a b", "mirror.example.com"} {
		if err := (Settings{DownloadURL: u, DefaultZone: "UTC", Maintenance: MaintenanceSettings{Check: CheckDaily, Retention: CheckDaily}}).validate(); err == nil ||
			!strings.Contains(err.Error(), "download_url") {
			t.Errorf("download_url %q: %v", u, err)
		}
	}
	for _, u := range []string{"https://mirror.example.com/holdfast", "file:///srv/holdfast/1.2.0"} {
		if err := (Settings{DownloadURL: u, DefaultZone: "UTC", Maintenance: MaintenanceSettings{Check: CheckDaily, Retention: CheckDaily}}).validate(); err != nil &&
			strings.Contains(err.Error(), "download_url") {
			t.Errorf("a good download_url %q is refused: %v", u, err)
		}
	}
}
