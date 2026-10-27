// Package apps implements the four GAS-style applications the playground
// exposes to workflows: MailApp.sendEmail (bundled send-only relay),
// MailApp.read (unsupported — the relay is send-only), UrlFetchApp.fetch
// (HTTP), and Logger.log (bundled Loki).
//
// The mail and logger backends are hard-wired through docker compose — the
// private `mailer` and `loki` services on the compose network, no
// credentials, exactly as an external Postfix/Loki would be. All side
// effects live here, in the Go server — never in the C core. The engine
// hands each bridge call to Service.Call with the calling user (the actor)
// so quotas and call records can be attributed.
package apps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"net/textproto"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/applogs"
	"github.com/tayyebi/flowcode-playground/server/internal/settings"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

// Hard-wired backends: compose-network services, no credentials, ports
// never published. The trust boundary is the docker network.
const (
	mailerAddr  = "mailer:25"
	lokiPushURL = "http://loki:3100/loki/api/v1/push"
	lokiTimeout = 5 * time.Second
)

// Actor is who a call is executed as: the logged-in user for manual runs,
// the project owner for triggers and public deployments.
type Actor struct {
	UserID    int64
	Email     string
	ProjectID int64
	ExecID    int64 // 0 until the execution row exists; used for Loki labels
}

// App names (system_settings / app_quotas keys).
const (
	AppMail   = "mail"
	AppHTTP   = "http"
	AppLogger = "logger"
)

// Outcome is what Service.Call produced.
type Outcome struct {
	// Result becomes the workflow's new token; empty means "leave the token
	// unchanged" (Logger.log's pass-through).
	Result string
	// Status is the human-readable call status for transcripts.
	Status string
	// App is the app family the call belonged to ("mail", "http", "logger").
	App string
	// Record is false for Logger (user entries live in Loki only, never in
	// app_calls; the call's operational line still reaches data/logs/logger.log).
	Record bool
	// Request/Response are capped summaries for the transcript.
	Request  string
	Response string
}

// Service executes app calls. It is safe for concurrent use.
type Service struct {
	settings *settings.Manager
	store    *store.Store
	http     *http.Client

	// Each app family's own operational log file — one line per dispatched
	// call, success or failure, surfaced in the admin area's Logs pages.
	oplogs map[string]*applogs.Logger

	mu      sync.Mutex
	windows map[string][]time.Time // key: userID|app, sliding 1-minute window
}

func New(st *store.Store, mgr *settings.Manager) *Service {
	return &Service{
		settings: mgr,
		store:    st,
		http:     &http.Client{},
		oplogs: map[string]*applogs.Logger{
			AppMail:   applogs.New(AppMail),
			AppHTTP:   applogs.New(AppHTTP),
			AppLogger: applogs.New(AppLogger),
		},
		windows: map[string][]time.Time{},
	}
}

// Call executes one app invocation. An error means the call failed: the
// engine reports it to the runner, which fails the workflow — strict
// semantics, uniformly for all four apps.
func (s *Service) Call(ctx context.Context, actor Actor, name string, params map[string]string, token []byte) (Outcome, error) {
	snap := s.settings.Current()
	start := time.Now()

	var app, status, result, request, response string
	var err error
	var record bool

	request = summarizeRequest(params, token)

	switch name {
	case "MailApp.sendEmail":
		app = AppMail
		record = true
		if !snap.Mail.Enabled {
			err = fmt.Errorf("mail app is disabled by the administrator")
			break
		}
		if qerr := s.allowQuota(ctx, actor.UserID, AppMail, snap.Quotas.MailPerMinute); qerr != nil {
			err = qerr
			break
		}
		status, response, err = s.sendEmail(ctx, snap.Mail, params, token)
	case "MailApp.read":
		app = AppMail
		record = true
		err = fmt.Errorf("mail app read is not supported — the bundled relay is send-only")
	case "UrlFetchApp.fetch":
		app = AppHTTP
		record = true
		if !snap.HTTP.Enabled {
			err = fmt.Errorf("http app is disabled by the administrator")
			break
		}
		if qerr := s.allowQuota(ctx, actor.UserID, AppHTTP, snap.Quotas.HTTPPerMinute); qerr != nil {
			err = qerr
			break
		}
		result, status, response, err = s.fetchURL(ctx, snap.HTTP, params, token)
	case "Logger.log":
		app = AppLogger
		record = false
		if !snap.Logger.Enabled {
			err = fmt.Errorf("logger app is disabled by the administrator")
			break
		}
		if qerr := s.allowQuota(ctx, actor.UserID, AppLogger, snap.Quotas.LoggerPerMinute); qerr != nil {
			err = qerr
			break
		}
		status, response, err = s.pushLoki(ctx, actor, name, params, token)
	default:
		err = fmt.Errorf("unknown app call %q", name)
	}

	var out Outcome
	if err != nil {
		out = Outcome{App: app, Status: "error: " + err.Error(), Record: record, Request: request}
	} else {
		out = Outcome{Result: result, Status: status, App: app, Record: record, Request: request, Response: response}
	}
	s.oplog(start, actor, name, out)
	if err != nil {
		return out, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// oplog appends the call's operational record to the app family's own log
// file — including the failures the switch produced before any side effect
// (disabled, unconfigured, quota-rejected). Logger entries carry the outcome
// and labels only: the user's log line lives in Loki, exactly as mail bodies
// live in the mail system, never in the platform's own logs.
func (s *Service) oplog(start time.Time, actor Actor, name string, out Outcome) {
	lg, ok := s.oplogs[out.App]
	if !ok || lg == nil {
		return // unknown app call — no family, no file
	}
	e := applogs.Entry{
		Name:       name,
		User:       firstNonEmpty(actor.Email, fmt.Sprintf("uid%d", actor.UserID)),
		ProjectID:  actor.ProjectID,
		ExecID:     actor.ExecID,
		Status:     out.Status,
		DurationMs: time.Since(start).Milliseconds(),
	}
	if out.App != AppLogger {
		e.Request = out.Request
		e.Response = out.Response
	}
	lg.Append(e)
}

/* ------------------------------------------------------------------ */
/* Quotas                                                              */
/* ------------------------------------------------------------------ */

func (s *Service) allowQuota(ctx context.Context, userID int64, app string, def int) error {
	limit, enabled, err := s.store.EffectiveQuota(ctx, userID, app, def)
	if err != nil {
		return fmt.Errorf("quota lookup: %w", err)
	}
	if !enabled {
		return fmt.Errorf("%s app is disabled for this user", app)
	}
	key := fmt.Sprintf("%d|%s", userID, app)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.windows[key][:0]
	for _, t := range s.windows[key] {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limit {
		s.windows[key] = kept
		return fmt.Errorf("%s app rate limit exceeded (%d/min)", app, limit)
	}
	s.windows[key] = append(kept, now)
	return nil
}

/* ------------------------------------------------------------------ */
/* MailApp.sendEmail — via the hard-wired bundled send-only relay      */
/* ------------------------------------------------------------------ */

func (s *Service) sendEmail(ctx context.Context, cfg settings.Mail, params map[string]string, token []byte) (status, response string, err error) {
	to := firstNonEmpty(params["to"], string(token))
	subject := params["subject"]
	if subject == "" {
		subject = "(no subject)"
	}
	body := params["body"]

	if to == "" {
		return "", "", fmt.Errorf("`to` parameter or a token is required")
	}
	from := cfg.From
	if from == "" {
		return "", "", fmt.Errorf("sender (from) is not configured — set it in /admin/mail")
	}

	msg := buildMailMessage(from, to, subject, body)

	// The bundled relay speaks plain SMTP on the private compose network —
	// no TLS, no credentials (the docker network is the trust boundary).
	c, cerr := smtp.Dial(mailerAddr)
	if cerr != nil {
		return "", "", fmt.Errorf("relay dial %s: %w", mailerAddr, cerr)
	}
	defer c.Close()

	if werr := c.Mail(from); werr != nil {
		return "", "", fmt.Errorf("relay MAIL: %w", werr)
	}
	if rerr := c.Rcpt(to); rerr != nil {
		return "", "", fmt.Errorf("relay RCPT: %w", rerr)
	}
	w, werr := c.Data()
	if werr != nil {
		return "", "", fmt.Errorf("relay DATA: %w", werr)
	}
	if _, werr = io.WriteString(w, msg); werr != nil {
		return "", "", fmt.Errorf("relay write: %w", werr)
	}
	if werr = w.Close(); werr != nil {
		return "", "", fmt.Errorf("relay close data: %w", werr)
	}
	if qerr := c.Quit(); qerr != nil {
		return "", "", fmt.Errorf("relay quit: %w", qerr)
	}

	st := fmt.Sprintf("sent to %s", to)
	return st, clamp(fmt.Sprintf("from=%s subject=%q", from, subject), 200), nil
}

func buildMailMessage(from, to, subject, body string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	b.WriteString(body)
	return b.String()
}

/* ------------------------------------------------------------------ */
/* UrlFetchApp.fetch                                                   */
/* ------------------------------------------------------------------ */

func (s *Service) fetchURL(ctx context.Context, cfg settings.HTTP, params map[string]string, token []byte) (result, status, response string, err error) {
	target := firstNonEmpty(params["url"], string(token))
	if target == "" {
		return "", "", "", fmt.Errorf("`url` parameter or a token is required")
	}
	u, perr := url.Parse(target)
	if perr != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", "", fmt.Errorf("url must be absolute http(s)")
	}
	if len(cfg.AllowedDomains) > 0 && !domainAllowed(u.Hostname(), cfg.AllowedDomains) {
		return "", "", "", fmt.Errorf("domain %q is not in the allowlist", u.Hostname())
	}

	method := strings.ToUpper(firstNonEmpty(params["method"], "GET"))
	body := params["body"]
	if body == "" && (method == "GET" || method == "HEAD") {
		body = ""
	}

	req, rerr := http.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
	if rerr != nil {
		return "", "", "", rerr
	}
	// Every param that is not url/method/body is a request header — the
	// language already admits dashed header names like `If-Modified-Since`.
	for k, v := range params {
		if k == "url" || k == "method" || k == "body" {
			continue
		}
		req.Header.Set(textproto.CanonicalMIMEHeaderKey(k), v)
	}

	timeout := cfg.TimeoutSeconds
	if timeout <= 0 {
		timeout = 10
	}
	s.http.Timeout = time.Duration(timeout) * time.Second

	resp, derr := s.http.Do(req)
	if derr != nil {
		return "", "", "", fmt.Errorf("fetch: %w", derr)
	}
	defer resp.Body.Close()

	cap := cfg.ResponseCapBytes
	if cap <= 0 {
		cap = 64 * 1024
	}
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, int64(cap)+1))
	truncated := len(buf) > cap
	if truncated {
		buf = buf[:cap]
	}

	st := fmt.Sprintf("%d %s", resp.StatusCode, resp.Status[strings.Index(resp.Status, " ")+1:])
	if truncated {
		st += " (body capped)"
	}
	return string(buf), st, clamp(fmt.Sprintf("%s %s → %s", method, target, st), 300), nil
}

func domainAllowed(host string, allowed []string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if a == "*" || a == h || strings.HasPrefix(a, "*.") {
			suffix := strings.TrimPrefix(a, "*")
			if strings.HasSuffix(h, suffix) {
				return true
			}
		} else if h == a {
			return true
		}
	}
	return false
}

/* ------------------------------------------------------------------ */
/* Logger.log → the hard-wired bundled Loki                           */
/* ------------------------------------------------------------------ */

func (s *Service) pushLoki(ctx context.Context, actor Actor, name string, params map[string]string, token []byte) (status, response string, err error) {
	message := firstNonEmpty(params["message"], string(token))
	if message == "" {
		message = "(empty)"
	}

	labels := map[string]string{
		"app":     name,
		"user":    labelSafe(firstNonEmpty(actor.Email, fmt.Sprintf("uid%d", actor.UserID))),
		"project": fmt.Sprintf("%d", actor.ProjectID),
		"exec":    fmt.Sprintf("%d", actor.ExecID),
	}
	line, _ := json.Marshal(map[string]any{
		"message": message,
		"name":    name,
		"user":    actor.Email,
	})

	// Loki wants nanosecond timestamps as strings.
	payload := map[string]any{
		"streams": []map[string]any{{
			"stream": labels,
			"values": [][]string{{fmt.Sprintf("%d", time.Now().UnixNano()), string(line)}},
		}},
	}
	raw, _ := json.Marshal(payload)

	reqCtx, cancel := context.WithTimeout(ctx, lokiTimeout)
	defer cancel()

	req, rerr := http.NewRequestWithContext(reqCtx, http.MethodPost, lokiPushURL, bytes.NewReader(raw))
	if rerr != nil {
		return "", "", rerr
	}
	req.Header.Set("Content-Type", "application/json")

	resp, derr := s.http.Do(req)
	if derr != nil {
		return "", "", fmt.Errorf("loki push: %w", derr)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
	if resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("loki push: HTTP %d", resp.StatusCode)
	}
	return "logged", clamp(message, 200), nil
}

func labelSafe(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

/* ------------------------------------------------------------------ */
/* Helpers                                                             */
/* ------------------------------------------------------------------ */

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func clamp(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func summarizeRequest(params map[string]string, token []byte) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%q", k, clamp(params[k], 120))
	}
	if len(token) > 0 {
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "token=%q", clamp(string(token), 120))
	}
	return clamp(b.String(), 400)
}
