package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kabsdk/holdfast/internal/access"
	"github.com/kabsdk/holdfast/internal/server/store"
)

// The server's own alerts. Every minute it looks at what it knows is wrong, the Problems page's
// list and its own problems, and sends, by mail, a webhook or both, the problems that have lasted notify.after and
// the ones that ended. After each pass it calls notify.heartbeat_url, so a service outside the site notices when the
// server stops: that is the dead man's switch. Prometheus's metrics and rules stay, for teams that have them.

// notifyState is what the last pass found, for the metrics.
type notifyState struct {
	mu       sync.Mutex
	lastOK   map[string]time.Time // by channel: mail, webhook, heartbeat
	failed   map[string]bool
	lastPass time.Time
}

// NotifyProblem is one problem in an alert.
type NotifyProblem struct {
	Code     string    `json:"code"`
	Severity string    `json:"severity"`
	Host     string    `json:"host,omitempty"`
	JobID    string    `json:"job_id,omitempty"`
	Message  string    `json:"message"`
	Since    time.Time `json:"since"`
}

// Alert is what is sent: the problems that started, and the ones that ended.
type Alert struct {
	Server  string          `json:"server"`
	URL     string          `json:"url,omitempty"` // the UI, when it is on
	Started []NotifyProblem `json:"started"`
	Ended   []NotifyProblem `json:"ended"`
	Text    string          `json:"text"` // a summary, the field Slack and Mattermost show
}

func problemKey(code, host, job string) string { return code + "|" + host + "|" + job }

// currentProblems is everything the server knows is wrong now: each host's, and its own.
func (s *Server) currentProblems(ctx context.Context) (map[string]NotifyProblem, error) {
	hosts, err := s.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]NotifyProblem{}
	for _, h := range hosts {
		for _, p := range h.Problems {
			out[problemKey(p.Code, h.FQDN, p.JobID)] = NotifyProblem{Code: p.Code, Severity: p.Severity, Host: h.FQDN, JobID: p.JobID, Message: p.Message}
		}
	}
	for _, p := range s.serverProblems(ctx) {
		out[problemKey(p.Code, "", "")] = NotifyProblem{Code: p.Code, Severity: p.Severity, Message: p.Message}
	}
	return out, nil
}

// notifyDue is one pass, from the maintenance loop.
func (s *Server) notifyDue(ctx context.Context) {
	if s.set.IsVault() {
		return
	}
	n := s.set.Notify
	ok := true
	if n.on() {
		ok = s.notifyPass(ctx) == nil
	}
	if n.HeartbeatURL != "" && ok {
		s.record("heartbeat", s.heartbeat(ctx))
	}
	s.notes.mu.Lock()
	s.notes.lastPass = s.now()
	s.notes.mu.Unlock()
}

func (s *Server) notifyPass(ctx context.Context) error {
	now := s.now()
	cur, err := s.currentProblems(ctx)
	if err != nil {
		s.log.Warn("alerts: reading the problems", "err", err)
		return err
	}
	q := s.st.Q()
	seen, err := q.NotifyState(ctx)
	if err != nil {
		s.log.Warn("alerts: reading what was sent", "err", err)
		return err
	}
	var a Alert
	var started, ended, quiet []string
	for k, p := range cur {
		row, ok := seen[k]
		if !ok {
			row = store.NotifyRow{Key: k, FirstSeen: now}
		}
		if err := q.SeeProblem(ctx, store.NotifyRow{Key: k, Severity: p.Severity, Message: p.Message, FirstSeen: row.FirstSeen}); err != nil {
			return err
		}
		if !row.Notified && now.Sub(row.FirstSeen) >= s.set.Notify.After {
			p.Since = row.FirstSeen
			a.Started = append(a.Started, p)
			started = append(started, k)
		}
	}
	for k, row := range seen {
		if _, still := cur[k]; still {
			continue
		}
		if !row.Notified {
			quiet = append(quiet, k) // it ended before it was sent: nothing to say
			continue
		}
		code, host, job := splitKey(k)
		a.Ended = append(a.Ended, NotifyProblem{Code: code, Severity: row.Severity, Host: host, JobID: job, Message: row.Message, Since: row.FirstSeen})
		ended = append(ended, k)
	}
	if len(quiet) > 0 {
		if err := q.ForgetProblems(ctx, quiet); err != nil {
			return err
		}
	}
	if len(a.Started)+len(a.Ended) == 0 {
		return nil
	}
	if err := s.send(ctx, a); err != nil {
		return err
	}
	if err := q.MarkNotified(ctx, started); err != nil {
		return err
	}
	return q.ForgetProblems(ctx, ended)
}

func splitKey(k string) (code, host, job string) {
	parts := strings.SplitN(k, "|", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	return parts[0], parts[1], parts[2]
}

// send sends an alert by every channel that is set. It fails if any channel failed, so the pass sends again next time.
func (s *Server) send(ctx context.Context, a Alert) error {
	sortProblems(a.Started)
	sortProblems(a.Ended)
	a.Server, a.URL = s.set.NodeName, s.set.UI.PublicURL
	a.Text = alertSummary(a)
	var errs []error
	if s.set.Notify.Mail != nil {
		err := s.sendMail(ctx, alertSubject(a), alertBody(a))
		s.record("mail", err)
		errs = append(errs, err)
	}
	if s.set.Notify.WebhookURL != "" {
		err := s.sendWebhook(ctx, a)
		s.record("webhook", err)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func sortProblems(ps []NotifyProblem) {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Severity != ps[j].Severity {
			return ps[i].Severity == "error"
		}
		if ps[i].Host != ps[j].Host {
			return ps[i].Host < ps[j].Host
		}
		return ps[i].Code < ps[j].Code
	})
}

func (s *Server) record(channel string, err error) {
	s.notes.mu.Lock()
	defer s.notes.mu.Unlock()
	if s.notes.lastOK == nil {
		s.notes.lastOK, s.notes.failed = map[string]time.Time{}, map[string]bool{}
	}
	s.notes.failed[channel] = err != nil
	if err == nil {
		s.notes.lastOK[channel] = s.now()
		return
	}
	s.log.Warn("alerts: sending failed", "channel", channel, "err", err)
}

func where(p NotifyProblem) string {
	switch {
	case p.Host == "":
		return "the server"
	case p.JobID == "":
		return p.Host
	}
	return p.Host + "/" + p.JobID
}

func alertSubject(a Alert) string {
	var parts []string
	if n := len(a.Started); n > 0 {
		parts = append(parts, fmt.Sprintf("%d new", n))
	}
	if n := len(a.Ended); n > 0 {
		parts = append(parts, fmt.Sprintf("%d resolved", n))
	}
	first := ""
	if len(a.Started) > 0 {
		first = ": " + a.Started[0].Code + " on " + where(a.Started[0])
	} else if len(a.Ended) > 0 {
		first = ": " + a.Ended[0].Code + " on " + where(a.Ended[0])
	}
	return fmt.Sprintf("[Holdfast %s] %s%s", a.Server, strings.Join(parts, ", "), first)
}

func alertSummary(a Alert) string {
	var b strings.Builder
	for _, p := range a.Started {
		fmt.Fprintf(&b, "%s %s on %s: %s\n", strings.ToUpper(p.Severity), p.Code, where(p), p.Message)
	}
	for _, p := range a.Ended {
		fmt.Fprintf(&b, "RESOLVED %s on %s\n", p.Code, where(p))
	}
	return strings.TrimSpace(b.String())
}

func alertBody(a Alert) string {
	var b strings.Builder
	if len(a.Started) > 0 {
		b.WriteString("Started:\n")
		for _, p := range a.Started {
			fmt.Fprintf(&b, "  %-7s %s on %s, since %s\n          %s\n", p.Severity, p.Code, where(p), p.Since.Format("2006-01-02 15:04 MST"), p.Message)
		}
		b.WriteString("\n")
	}
	if len(a.Ended) > 0 {
		b.WriteString("Resolved:\n")
		for _, p := range a.Ended {
			fmt.Fprintf(&b, "  %s on %s, which started %s\n", p.Code, where(p), p.Since.Format("2006-01-02 15:04 MST"))
		}
		b.WriteString("\n")
	}
	if a.URL != "" {
		fmt.Fprintf(&b, "The Problems page: %s/#/problems\n", strings.TrimSuffix(a.URL, "/"))
	}
	fmt.Fprintf(&b, "From the Holdfast server %s. Each problem is sent once when it has lasted a while, and once when it ends.\n", a.Server)
	return b.String()
}

// sendMail sends one plain-text mail through the relay: STARTTLS, TLS from the start, or none.
func (s *Server) sendMail(ctx context.Context, subject, body string) error {
	m := s.set.Notify.Mail
	host, _, err := net.SplitHostPort(m.SMTP)
	if err != nil {
		return fmt.Errorf("notify.mail.smtp %q: %w", m.SMTP, err)
	}
	d := net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	if m.TLS == "tls" {
		conn, err = tls.DialWithDialer(&d, "tcp", m.SMTP, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", m.SMTP)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(time.Minute))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if m.TLS == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("the relay %s does not offer STARTTLS; set notify.mail.tls to tls or none", m.SMTP)
		}
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if m.Username != "" {
		pw, err := os.ReadFile(m.PasswordFile)
		if err != nil {
			return fmt.Errorf("notify.mail.password_file: %w", err)
		}
		if err := c.Auth(smtp.PlainAuth("", m.Username, strings.TrimSpace(string(pw)), host)); err != nil {
			return err
		}
	}
	if err := c.Mail(m.From); err != nil {
		return err
	}
	for _, to := range m.To {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s",
		m.From, strings.Join(m.To, ", "), subject, time.Now().Format(time.RFC1123Z), strings.ReplaceAll(body, "\n", "\r\n"))
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func (s *Server) sendWebhook(ctx context.Context, a Alert) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.set.Notify.WebhookURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the webhook answered %s", resp.Status)
	}
	return nil
}

func (s *Server) heartbeat(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.set.Notify.HeartbeatURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the heartbeat URL answered %s", resp.Status)
	}
	return nil
}

// NotifyTestResult says, per channel, whether a test message went.
type NotifyTestResult struct {
	Mail      string `json:"mail,omitempty"`
	Webhook   string `json:"webhook,omitempty"`
	Heartbeat string `json:"heartbeat,omitempty"`
}

// NotifyTest sends a test message by every channel that is set (an admin's), and says how each went.
func (s *Server) NotifyTest(ctx context.Context, a Actor) (NotifyTestResult, error) {
	if err := s.requireEverywhere(ctx, a, access.Maintenance); err != nil {
		return NotifyTestResult{}, err
	}
	n := s.set.Notify
	if !n.on() && n.HeartbeatURL == "" {
		return NotifyTestResult{}, fmt.Errorf("no alert is set up: add notify.mail, notify.webhook_url or notify.heartbeat_url to server.yaml: %w", ErrConflict)
	}
	outcome := func(err error) string {
		if err != nil {
			return "failed: " + err.Error()
		}
		return "sent"
	}
	test := Alert{Server: s.set.NodeName, URL: s.set.UI.PublicURL, Started: []NotifyProblem{}, Ended: []NotifyProblem{}}
	test.Text = "Holdfast test alert from " + s.set.NodeName + ", sent by " + a.Name + ": this channel works."
	var res NotifyTestResult
	if n.Mail != nil {
		res.Mail = outcome(s.sendMail(ctx, "[Holdfast "+s.set.NodeName+"] test alert", test.Text+"\n"))
	}
	if n.WebhookURL != "" {
		res.Webhook = outcome(s.sendWebhook(ctx, test))
	}
	if n.HeartbeatURL != "" {
		res.Heartbeat = outcome(s.heartbeat(ctx))
	}
	err := s.st.Q().Audit(ctx, store.AuditEntry{Actor: a.Name, Source: a.Source, Action: "notify.test",
		Detail: map[string]any{"mail": res.Mail, "webhook": res.Webhook, "heartbeat": res.Heartbeat}})
	return res, err
}

func (s *Server) apiNotifyTest(w http.ResponseWriter, r *http.Request) {
	res, err := s.NotifyTest(r.Context(), actorOf(r))
	if err != nil {
		apiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// writeNotifyMetrics adds the alerts' own health to /metrics.
func (s *Server) writeNotifyMetrics(b *strings.Builder) {
	s.notes.mu.Lock()
	defer s.notes.mu.Unlock()
	if len(s.notes.failed) == 0 {
		return
	}
	fmt.Fprintf(b, "# HELP holdfast_notify_failed 1 if the server's last alert or heartbeat by this channel failed.\n# TYPE holdfast_notify_failed gauge\n")
	for _, ch := range []string{"mail", "webhook", "heartbeat"} {
		if f, ok := s.notes.failed[ch]; ok {
			v := 0
			if f {
				v = 1
			}
			fmt.Fprintf(b, "holdfast_notify_failed{channel=%q} %d\n", ch, v)
		}
	}
}
