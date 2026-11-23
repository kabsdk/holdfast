package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/kabsdk/holdfast/internal/config"
)

// A job in the API may leave things out (plan-m10 item 4): a new job takes what it does not give from the default job
// (daily at 02:00 with a 20-minute spread; one a day for 14 days, a week for 8 weeks, a month for 12 months, a year for
// 3 years; the newest 3), and an existing job keeps its own. So `holdfast job set web01 system --path /etc` is a whole
// job, and `holdfast job set web01 system --keep-daily 30d` changes the retention alone.

// jobBody is PUT /api/v1/hosts/{fqdn}/jobs/{job}'s body, with the job as given.
type jobBody struct {
	Base    *int64          `json:"base_revision,omitempty"`
	Message string          `json:"message"`
	Job     json.RawMessage `json:"job"`
	Accept  []string        `json:"accept,omitempty"`
}

// jobFrom is the job a request means: what it gives, on top of the job as it is, or the default job for a new one.
func (s *Server) jobFrom(ctx context.Context, fqdn, jobID string, given json.RawMessage) (config.Job, error) {
	head, err := s.head(ctx, s.st.Q())
	if err != nil {
		return config.Job{}, err
	}
	if h, ok := head.Config.Hosts[fqdn]; ok {
		if j, ok := h.Jobs[jobID]; ok {
			return mergeJob(j, given, false)
		}
	}
	return mergeJob(config.DefaultJob(), given, true)
}

// mergeJob lays the fields given over base. A schedule that names no form (daily, every, weekly) changes only what it
// gives, such as the spread or the zone. One that names a form replaces the form; an existing job keeps its spread and
// zone unless it gives them, while a new one (fresh, base the default job) takes the schedule given whole, as the
// default's spread suits the default's form only. The retention, when given, is replaced whole. Giving a dump drops
// base's paths, and giving paths drops its dump: a job backs up one or the other.
func mergeJob(base config.Job, given json.RawMessage, fresh bool) (config.Job, error) {
	var g map[string]json.RawMessage
	if len(bytes.TrimSpace(given)) > 0 && !bytes.Equal(bytes.TrimSpace(given), []byte("null")) {
		if err := json.Unmarshal(given, &g); err != nil {
			return config.Job{}, &ValidationError{config.Problems{{Path: "job", Message: "not a job: " + err.Error()}}}
		}
	}
	b, err := json.Marshal(base)
	if err != nil {
		return config.Job{}, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return config.Job{}, err
	}
	for k, v := range g {
		if k == "schedule" {
			if v, err = mergeSchedule(m["schedule"], v, fresh); err != nil {
				return config.Job{}, err
			}
		}
		m[k] = v
	}
	if _, ok := g["paths"]; !ok && hasValue(g, "dump") {
		m["paths"] = json.RawMessage("[]")
	}
	if _, ok := g["dump"]; !ok && hasValue(g, "paths") {
		delete(m, "dump")
	}
	merged, err := json.Marshal(m)
	if err != nil {
		return config.Job{}, err
	}
	var j config.Job
	dec := json.NewDecoder(bytes.NewReader(merged))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		return config.Job{}, &ValidationError{config.Problems{{Path: "job", Message: err.Error()}}}
	}
	if j.Engine == "" {
		j.Engine = config.EngineRestic
	}
	return j, nil
}

// hasValue says whether the request gives the field a value that is not empty.
func hasValue(g map[string]json.RawMessage, k string) bool {
	v, ok := g[k]
	if !ok {
		return false
	}
	s := string(bytes.TrimSpace(v))
	return s != "null" && s != `""` && s != "[]"
}

func mergeSchedule(base, given json.RawMessage, fresh bool) (json.RawMessage, error) {
	var b, g map[string]json.RawMessage
	if err := json.Unmarshal(given, &g); err != nil {
		return nil, &ValidationError{config.Problems{{Path: "job.schedule", Message: "not a schedule: " + err.Error()}}}
	}
	if err := json.Unmarshal(base, &b); err != nil || b == nil {
		b = map[string]json.RawMessage{}
	}
	if g["daily"] != nil || g["every"] != nil || g["weekly"] != nil {
		if fresh {
			b = map[string]json.RawMessage{}
		}
		delete(b, "daily")
		delete(b, "every")
		delete(b, "weekly")
	}
	for k, v := range g {
		b[k] = v
	}
	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("schedule: %w", err)
	}
	return out, nil
}

func (s *Server) apiSetJob(w http.ResponseWriter, r *http.Request) {
	var in jobBody
	if !decode(w, r, &in) {
		return
	}
	job, err := s.jobFrom(r.Context(), r.PathValue("fqdn"), r.PathValue("job"), in.Job)
	if err != nil {
		apiError(w, err)
		return
	}
	res, err := s.SetJob(r.Context(), actorOf(r), r.PathValue("fqdn"), r.PathValue("job"),
		SetJobInput{Base: in.Base, Message: in.Message, Job: job, Accept: in.Accept})
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiCheckJob(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Job    json.RawMessage `json:"job"`
		Accept []string        `json:"accept,omitempty"`
	}
	if !decode(w, r, &in) {
		return
	}
	job, err := s.jobFrom(r.Context(), r.PathValue("fqdn"), r.PathValue("job"), in.Job)
	if err != nil {
		apiError(w, err)
		return
	}
	res, err := s.CheckJob(r.Context(), r.PathValue("fqdn"), r.PathValue("job"), JobCheckInput{Job: job, Accept: in.Accept})
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
