package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/kabsdk/holdfast/internal/config"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// Restore both versions to diff on the host (spec §5.4): one path from two snapshots, each into
// a new directory only root can read, and the diff -u command to compare them there. Holdfast shows no
// contents (spec §6.5); the comparison runs on the host.

// RestorePairInput names the path and the other snapshot.
type RestorePairInput struct {
	Path    string `json:"path"`
	Against string `json:"against"`
	Message string `json:"message,omitempty"`
}

// RestorePairResult is the two queued restores: this snapshot's first, then the other's.
type RestorePairResult struct {
	Path      string    `json:"path"`
	Snapshots [2]string `json:"snapshots"`
	Commands  [2]string `json:"commands"`
}

// RestorePair queues both restores.
func (s *Server) RestorePair(ctx context.Context, a Actor, fqdn, id string, in RestorePairInput) (RestorePairResult, error) {
	if !strings.HasPrefix(in.Path, "/") || path.Clean(in.Path) == "/" {
		return RestorePairResult{}, &ValidationError{config.Problems{{Path: "path", Message: "give the absolute path of the file to compare"}}}
	}
	if strings.TrimSpace(in.Against) == "" {
		return RestorePairResult{}, &ValidationError{config.Problems{{Path: "against", Message: "give the other snapshot"}}}
	}
	res := RestorePairResult{Path: path.Clean(in.Path)}
	for i, sn := range []string{id, in.Against} {
		r, err := s.Restore(ctx, a, fqdn, sn, RestoreInput{Include: []string{res.Path}, Message: in.Message})
		if err != nil {
			return RestorePairResult{}, err
		}
		res.Snapshots[i], res.Commands[i] = r.Snapshot, r.Command
	}
	return res, nil
}

// PairStatus says whether both restores are done and, once they are, how to compare them on the host.
type PairStatus struct {
	Status      string    `json:"status"` // queued, done or failed
	Directories [2]string `json:"directories,omitempty"`
	Diff        string    `json:"diff,omitempty"` // the command to run on the host, as root
	Error       string    `json:"error,omitempty"`
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// RestorePairStatus looks up the two restores by their commands.
func (s *Server) RestorePairStatus(ctx context.Context, fqdn string, commands [2]string, file string) (PairStatus, error) {
	st := PairStatus{Status: "done"}
	for i, id := range commands {
		ev, err := s.st.Q().RestoreEvent(ctx, fqdn, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			st.Status = "queued"
			continue
		case err != nil:
			return PairStatus{}, err
		case ev.Status != "success":
			return PairStatus{Status: "failed", Error: fmt.Sprintf("the restore %s: %s %s", id, ev.Status, ev.Error)}, nil
		}
		st.Directories[i] = ev.Detail["directory"]
	}
	if st.Status == "done" {
		st.Diff = "sudo diff -u " + shellQuote(path.Join(st.Directories[0], file)) + " " + shellQuote(path.Join(st.Directories[1], file))
	}
	return st, nil
}

func (s *Server) apiRestorePair(w http.ResponseWriter, r *http.Request) {
	var in RestorePairInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.RestorePair(r.Context(), actorOf(r), r.PathValue("fqdn"), r.PathValue("id"), in)
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) apiRestorePairStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	st, err := s.RestorePairStatus(r.Context(), r.PathValue("fqdn"), [2]string{q.Get("a"), q.Get("b")}, q.Get("path"))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
