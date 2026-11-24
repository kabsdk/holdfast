package gateway

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

var anID = strings.Repeat("ab", 32)

func TestParse(t *testing.T) {
	create := url.Values{"create": {"true"}}
	for _, tc := range []struct {
		method, path string
		query        url.Values
		want         Request
		key          string
	}{
		{"HEAD", Base + "web01.example.com/config", nil, Request{"web01.example.com", KindConfig, "", OpStat}, "repos/web01.example.com/config"},
		{"GET", Base + "web01.example.com/config", nil, Request{"web01.example.com", KindConfig, "", OpRead}, "repos/web01.example.com/config"},
		{"POST", Base + "web01.example.com/config", nil, Request{"web01.example.com", KindConfig, "", OpWrite}, "repos/web01.example.com/config"},
		{"POST", Base + "web01.example.com/", create, Request{"web01.example.com", "", "", OpCreate}, ""},
		{"GET", Base + "web01.example.com/data/", nil, Request{"web01.example.com", KindData, "", OpList}, "repos/web01.example.com/data/"},
		{"GET", Base + "web01.example.com/data/" + anID, nil, Request{"web01.example.com", KindData, anID, OpRead}, "repos/web01.example.com/data/ab/" + anID},
		{"POST", Base + "web01.example.com/snapshots/" + anID, nil, Request{"web01.example.com", KindSnapshots, anID, OpWrite}, "repos/web01.example.com/snapshots/" + anID},
		{"DELETE", Base + "web01.example.com/locks/" + anID, nil, Request{"web01.example.com", KindLocks, anID, OpDelete}, "repos/web01.example.com/locks/" + anID},
		{"HEAD", Base + "web01.example.com/index/" + anID, nil, Request{"web01.example.com", KindIndex, anID, OpStat}, "repos/web01.example.com/index/" + anID},
		{"GET", Base + "web01.example.com/keys/", nil, Request{"web01.example.com", KindKeys, "", OpList}, "repos/web01.example.com/keys/"},
	} {
		got, err := Parse(tc.method, tc.path, tc.query)
		if err != nil || got != tc.want {
			t.Errorf("%s %s: %+v %v, want %+v", tc.method, tc.path, got, err, tc.want)
			continue
		}
		if tc.key != "" && got.Key() != tc.key {
			t.Errorf("%s %s: key %s, want %s", tc.method, tc.path, got.Key(), tc.key)
		}
	}
}

// Nothing but restic's own requests is understood: no path trick, other name or other method reaches the bucket.
func TestParseRefusesTricks(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{"GET", "/gateway/v2/web01.example.com/config"},
		{"GET", Base + "../escrow/web01.example.com.age"},
		{"GET", Base + "web01.example.com/../db01.example.com/config"},
		{"GET", Base + "web01.example.com/data/../../db01.example.com/config"},
		{"GET", Base + "web01.example.com%2F..%2Fdb01.example.com/config"},
		{"GET", Base + "web01.example.com/data%2F" + anID},
		{"GET", Base + "web01.example.com/data/" + strings.ToUpper(anID)},
		{"GET", Base + "web01.example.com/data/" + anID[:63]},
		{"GET", Base + "web01.example.com/data/" + anID + "/x"},
		{"GET", Base + "web01.example.com/data/ab/" + anID},
		{"GET", Base + "web01.example.com/escrow/" + anID},
		{"GET", Base + "Web01.example.com/config"},
		{"GET", Base + "Web01/config"}, // an agent's name is lower case: web01 alone is one (decision 056)
		{"GET", Base + "web01.example.com"},
		{"GET", Base + "web01.example.com/"},
		{"POST", Base + "web01.example.com/"},
		{"DELETE", Base + "web01.example.com/config"},
		{"POST", Base + "web01.example.com/data/"},
		{"PUT", Base + "web01.example.com/data/" + anID},
		{"PATCH", Base + "web01.example.com/locks/" + anID},
		{"GET", Base + "web01.example.com\\..\\x/config"},
	} {
		if r, err := Parse(tc.method, tc.path, nil); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s %s was understood as %+v", tc.method, tc.path, r)
		}
	}
}

func TestCheck(t *testing.T) {
	req := func(op Op, kind string) Request {
		return Request{Repo: "db01.example.com", Kind: kind, Name: anID, Op: op}
	}
	reason := func(err error) string {
		var r *Refusal
		if errors.As(err, &r) {
			return r.Reason
		}
		return ""
	}
	for _, tc := range []struct {
		r      Request
		a      Access
		reason string // "" when allowed
	}{
		// Its own repository: read anything, add data, index, snapshots and locks, delete only locks.
		{req(OpRead, KindData), Own, ""},
		{req(OpStat, KindConfig), Own, ""},
		{req(OpList, KindKeys), Own, ""},
		{req(OpWrite, KindData), Own, ""},
		{req(OpWrite, KindIndex), Own, ""},
		{req(OpWrite, KindSnapshots), Own, ""},
		{req(OpWrite, KindLocks), Own, ""},
		{req(OpDelete, KindLocks), Own, ""},
		{req(OpDelete, KindData), Own, ReasonDelete},
		{req(OpDelete, KindSnapshots), Own, ReasonDelete},
		{req(OpDelete, KindIndex), Own, ReasonDelete},
		{req(OpDelete, KindKeys), Own, ReasonDelete},
		{req(OpWrite, KindKeys), Own, ReasonServerOwned},
		{req(OpWrite, KindConfig), Own, ReasonServerOwned},
		{Request{Repo: "db01.example.com", Op: OpCreate}, Own, ReasonServerOwned},
		// Under a grant: read only, not even a lock.
		{req(OpRead, KindData), ReadOnly, ""},
		{req(OpList, KindSnapshots), ReadOnly, ""},
		{req(OpStat, KindConfig), ReadOnly, ""},
		{req(OpWrite, KindLocks), ReadOnly, ReasonReadOnly},
		{req(OpDelete, KindLocks), ReadOnly, ReasonReadOnly},
		{req(OpWrite, KindSnapshots), ReadOnly, ReasonReadOnly},
		// Another host's repository: nothing.
		{req(OpRead, KindConfig), NoAccess, ReasonOtherRepository},
		{req(OpList, KindSnapshots), NoAccess, ReasonOtherRepository},
		{req(OpWrite, KindData), NoAccess, ReasonOtherRepository},
		{req(OpDelete, KindLocks), NoAccess, ReasonOtherRepository},
	} {
		if got := reason(Check(tc.r, tc.a)); got != tc.reason {
			t.Errorf("%s %s with access %d: refused for %q, want %q", tc.r.Op, tc.r.Kind, tc.a, got, tc.reason)
		}
	}
}
