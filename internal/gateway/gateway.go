package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MaxUpload is the largest file a host may write. restic's packs are at most 128 MiB.
const MaxUpload = 512 << 20

// Options build a gateway.
type Options struct {
	Base                         string // where it is served; Base when empty
	Endpoint, Bucket, Region     string
	AccessKeyID, SecretAccessKey string // the gateway key (docs/storage.md)
	// Access says what host may do with repo's repository: Own for its own, ReadOnly under a grant.
	Access func(ctx context.Context, host, repo string) Access
	// Refused is told of each refusal, at most once a minute for the same host, repository and reason: the server
	// audits it. Every refusal is counted in the metrics.
	Refused func(ctx context.Context, host string, r Request, refusal *Refusal)
	Log     *slog.Logger
}

// Gateway serves restic's REST protocol for the hosts' repositories in one bucket.
type Gateway struct {
	o      Options
	client *minio.Client
	core   minio.Core

	mu       sync.Mutex
	hosts    map[string]*counters
	lastSaid map[string]time.Time // when a refusal was last passed to Refused
}

type counters struct {
	requests map[Op]int64
	in, out  int64
	refusals map[string]int64
}

// New builds a gateway. It does not contact the bucket.
func New(o Options) (*Gateway, error) {
	u, err := url.Parse(o.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("storage.endpoint %q is not a URL", o.Endpoint)
	}
	c, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(o.AccessKeyID, o.SecretAccessKey, ""), Secure: u.Scheme == "https", Region: o.Region})
	if err != nil {
		return nil, err
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Base == "" {
		o.Base = Base
	}
	return &Gateway{o: o, client: c, core: minio.Core{Client: c}, hosts: map[string]*counters{}, lastSaid: map[string]time.Time{}}, nil
}

// Serve answers one request from host, the host whose certificate the connection was made with.
func (g *Gateway) Serve(w http.ResponseWriter, r *http.Request, host string) {
	ctx := r.Context()
	req, err := ParseAt(g.o.Base, r.Method, r.URL.EscapedPath(), r.URL.Query())
	if err != nil {
		g.refuse(ctx, w, host, Request{Repo: "", Op: Op(strings.ToLower(r.Method))}, &Refusal{ReasonMalformed, err.Error()}, http.StatusBadRequest)
		return
	}
	if err := Check(req, g.o.Access(ctx, host, req.Repo)); err != nil {
		ref, status := err.(*Refusal), http.StatusForbidden
		if ref.Reason == ReasonRollback {
			// For a while only: restic retries, and a backup that still meets it fails, and is retried later.
			status = http.StatusServiceUnavailable
			w.Header().Set("Retry-After", "60")
		}
		g.refuse(ctx, w, host, req, ref, status)
		return
	}
	g.count(host, func(c *counters) { c.requests[req.Op]++ })
	switch req.Op {
	case OpStat:
		g.stat(w, r, req)
	case OpRead:
		g.read(w, r, host, req)
	case OpList:
		g.list(w, r, req)
	case OpWrite:
		g.write(w, r, host, req)
	case OpDelete:
		g.delete(w, r, req)
	}
}

func (g *Gateway) refuse(ctx context.Context, w http.ResponseWriter, host string, req Request, ref *Refusal, status int) {
	g.count(host, func(c *counters) { c.refusals[ref.Reason]++ })
	key := host + "|" + req.Repo + "|" + ref.Reason
	g.mu.Lock()
	say := time.Since(g.lastSaid[key]) > time.Minute
	if say {
		g.lastSaid[key] = time.Now()
	}
	g.mu.Unlock()
	if say && g.o.Refused != nil {
		g.o.Refused(ctx, host, req, ref)
	}
	http.Error(w, "refused by the Holdfast gateway: "+ref.Message, status)
}

func (g *Gateway) stat(w http.ResponseWriter, r *http.Request, req Request) {
	info, err := g.client.StatObject(r.Context(), g.o.Bucket, req.Key(), minio.StatObjectOptions{})
	if err != nil {
		g.s3Error(w, req, err)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
}

// byteRange is the Range restic sends: from an offset, to an end or to the end of the file.
var byteRange = regexp.MustCompile(`^bytes=[0-9]+-[0-9]*$`)

func (g *Gateway) read(w http.ResponseWriter, r *http.Request, host string, req Request) {
	var opts minio.GetObjectOptions
	if rg := r.Header.Get("Range"); rg != "" {
		if !byteRange.MatchString(rg) {
			http.Error(w, "the gateway takes a range as restic sends it: bytes=<from>-[<to>]", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		opts.Set("Range", rg)
	}
	body, info, hdr, err := g.core.GetObject(r.Context(), g.o.Bucket, req.Key(), opts)
	if err != nil {
		g.s3Error(w, req, err)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	status := http.StatusOK
	if cr := hdr.Get("Content-Range"); cr != "" {
		w.Header().Set("Content-Range", cr)
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	n, _ := io.Copy(w, body)
	g.count(host, func(c *counters) { c.out += n })
}

const (
	contentV1 = "application/vnd.x.restic.rest.v1"
	contentV2 = "application/vnd.x.restic.rest.v2"
)

func (g *Gateway) list(w http.ResponseWriter, r *http.Request, req Request) {
	type entry struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	entries := []entry{}
	for o := range g.client.ListObjects(r.Context(), g.o.Bucket, minio.ListObjectsOptions{Prefix: req.Key(), Recursive: true}) {
		if o.Err != nil {
			g.s3Error(w, req, o.Err)
			return
		}
		// Only restic's own files, where restic puts them: never a probe object or anything else under the prefix.
		name := path.Base(o.Key)
		if !id.MatchString(name) || (Request{Repo: req.Repo, Kind: req.Kind, Name: name}).Key() != o.Key {
			continue
		}
		entries = append(entries, entry{Name: name, Size: o.Size})
	}
	if strings.Contains(r.Header.Get("Accept"), contentV2) {
		w.Header().Set("Content-Type", contentV2)
		_ = json.NewEncoder(w).Encode(entries)
		return
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	w.Header().Set("Content-Type", contentV1)
	_ = json.NewEncoder(w).Encode(names)
}

func (g *Gateway) write(w http.ResponseWriter, r *http.Request, host string, req Request) {
	switch {
	case r.ContentLength < 0:
		http.Error(w, "the gateway needs the file's length", http.StatusLengthRequired)
		return
	case r.ContentLength > MaxUpload:
		http.Error(w, "the file is larger than the gateway takes", http.StatusRequestEntityTooLarge)
		return
	}
	ctx, key := r.Context(), req.Key()
	// A file that exists is never written again. restic names a file by its content, so the one there holds what
	// this upload holds, as when restic sends a file again after a lost answer; and a host cannot overwrite its
	// backups with something else.
	if _, err := g.client.StatObject(ctx, g.o.Bucket, key, minio.StatObjectOptions{}); err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, r.ContentLength))
		w.WriteHeader(http.StatusOK)
		return
	} else if !notFound(err) {
		g.s3Error(w, req, err)
		return
	}
	// One PUT, streamed: PartSize above MaxUpload keeps minio-go from buffering parts in memory.
	info, err := g.client.PutObject(ctx, g.o.Bucket, key, http.MaxBytesReader(w, r.Body, MaxUpload), r.ContentLength,
		minio.PutObjectOptions{ContentType: "application/octet-stream", PartSize: MaxUpload + 1<<20})
	if err != nil {
		g.s3Error(w, req, err)
		return
	}
	g.count(host, func(c *counters) { c.in += info.Size })
	w.WriteHeader(http.StatusOK)
}

func (g *Gateway) delete(w http.ResponseWriter, r *http.Request, req Request) {
	if err := g.client.RemoveObject(r.Context(), g.o.Bucket, req.Key(), minio.RemoveObjectOptions{}); err != nil {
		g.s3Error(w, req, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func notFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound
}

func (g *Gateway) s3Error(w http.ResponseWriter, req Request, err error) {
	switch {
	case notFound(err):
		http.Error(w, "not found", http.StatusNotFound)
	case minio.ToErrorResponse(err).Code == "InvalidRange":
		http.Error(w, "the range is outside the file", http.StatusRequestedRangeNotSatisfiable)
	default:
		g.o.Log.Warn("the gateway's S3 request failed", "repository", req.Repo, "op", req.Op, "kind", req.Kind, "err", err)
		http.Error(w, "the bucket did not answer the gateway; it retries", http.StatusBadGateway)
	}
}

func (g *Gateway) count(host string, f func(*counters)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.hosts[host]
	if c == nil {
		c = &counters{requests: map[Op]int64{}, refusals: map[string]int64{}}
		g.hosts[host] = c
	}
	f(c)
}

// WriteMetrics writes the gateway's counters in Prometheus text format. They start again from 0 when the server
// starts, as counters do.
func (g *Gateway) WriteMetrics(w io.Writer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	hosts := make([]string, 0, len(g.hosts))
	for h := range g.hosts {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	counter := func(name, help string) { fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name) }
	counter("holdfast_gateway_requests_total", "Requests the gateway answered for the host, by operation.")
	for _, h := range hosts {
		ops := make([]string, 0)
		for op := range g.hosts[h].requests {
			ops = append(ops, string(op))
		}
		sort.Strings(ops)
		for _, op := range ops {
			fmt.Fprintf(w, "holdfast_gateway_requests_total{host=%q,op=%q} %d\n", h, op, g.hosts[h].requests[Op(op)])
		}
	}
	counter("holdfast_gateway_bytes_total", "Bytes the host wrote (in) and read (out) through the gateway.")
	for _, h := range hosts {
		fmt.Fprintf(w, "holdfast_gateway_bytes_total{host=%q,direction=\"in\"} %d\n", h, g.hosts[h].in)
		fmt.Fprintf(w, "holdfast_gateway_bytes_total{host=%q,direction=\"out\"} %d\n", h, g.hosts[h].out)
	}
	counter("holdfast_gateway_refusals_total", "Requests the gateway refused the host, by reason.")
	for _, h := range hosts {
		reasons := make([]string, 0)
		for r := range g.hosts[h].refusals {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		for _, r := range reasons {
			fmt.Fprintf(w, "holdfast_gateway_refusals_total{host=%q,reason=%q} %d\n", h, r, g.hosts[h].refusals[r])
		}
	}
}
