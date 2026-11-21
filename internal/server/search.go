package server

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// SearchResult is what search finds (spec §5.6): hosts, jobs and snapshot IDs. A snapshot that is no longer
// in its repository is still found, and marked deleted (by Holdfast) or disappeared (outside it).
type SearchResult struct {
	Hosts     []string         `json:"hosts"`
	Jobs      []SearchJob      `json:"jobs"`
	Snapshots []SearchSnapshot `json:"snapshots"`
}

// SearchJob is one job a search found.
type SearchJob struct {
	FQDN  string `json:"fqdn"`
	JobID string `json:"job_id"`
}

// SearchSnapshot is one snapshot a search found.
type SearchSnapshot struct {
	store.Snapshot
	State string `json:"state"` // present, deleted or disappeared
}

var hexPrefix = regexp.MustCompile(`^[0-9a-f]{4,64}$`)

// Search finds hosts and jobs whose name contains q, and snapshots whose ID starts with q (4 hex digits or
// more).
func (s *Server) Search(ctx context.Context, q string) (SearchResult, error) {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" || len(q) > 255 {
		return SearchResult{}, &ValidationError{config.Problems{{Path: "q", Message: "give part of a host name, a job or a snapshot ID"}}}
	}
	res := SearchResult{Hosts: []string{}, Jobs: []SearchJob{}, Snapshots: []SearchSnapshot{}}
	st := s.st.Q()
	hosts, err := st.ListHosts(ctx)
	if err != nil {
		return res, err
	}
	for _, h := range hosts {
		if strings.Contains(h.FQDN, q) {
			res.Hosts = append(res.Hosts, h.FQDN)
		}
	}
	head, err := s.head(ctx, st)
	if err != nil {
		return res, err
	}
	for fqdn, h := range head.Config.Hosts {
		for id := range h.Jobs {
			if strings.Contains(id, q) {
				res.Jobs = append(res.Jobs, SearchJob{FQDN: fqdn, JobID: id})
			}
		}
	}
	sort.Slice(res.Jobs, func(i, j int) bool {
		return res.Jobs[i].FQDN+"/"+res.Jobs[i].JobID < res.Jobs[j].FQDN+"/"+res.Jobs[j].JobID
	})
	if hexPrefix.MatchString(q) {
		found, err := st.SearchSnapshots(ctx, q, 100)
		if err != nil {
			return res, err
		}
		for _, sn := range found {
			state := "present"
			switch {
			case sn.GoneAt != nil && sn.DeletedByUs:
				state = "deleted"
			case sn.GoneAt != nil:
				state = "disappeared"
			}
			res.Snapshots = append(res.Snapshots, SearchSnapshot{Snapshot: sn, State: state})
		}
	}
	return res, nil
}
