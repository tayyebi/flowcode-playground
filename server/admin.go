package main

// The system administration area: a sidebar of pages grouped per app
// (Mail, HTTP, Logger — each with Configuration, Logs, Rate limits) plus a
// System group (Users, Executions audit, General). Gated on the manually
// promoted users.is_admin flag; there is no promote UI by design.
//
// Secrets (SMTP/IMAP passwords, Loki basic-auth) are write-only: forms never
// echo them back, and an empty field on save means "keep the current value".

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/settings"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

/* ------------------------------------------------------------------ */
/* Shared page shape                                                   */
/* ------------------------------------------------------------------ */

type adminPage struct {
	basePage
	Section string // sidebar highlight: "", "mail", "http", "logger", "system"
	OK      string
}

func (s *Server) adminBase(r *http.Request, title, section string) adminPage {
	b := s.basePage(r, title)
	return adminPage{basePage: b, Section: section}
}

/* ------------------------------------------------------------------ */
/* Home                                                                */
/* ------------------------------------------------------------------ */

func (s *Server) handleAdminHomePage(w http.ResponseWriter, r *http.Request) {
	snap := s.settings.Current()
	s.renderPage(w, http.StatusOK, "admin-home.gohtml", struct {
		adminPage
		Mail, HTTP, Logger bool
		Quotas             settings.Quotas
	}{s.adminBase(r, "Administration", ""), snap.Mail.Enabled, snap.HTTP.Enabled, snap.Logger.Enabled, snap.Quotas})
}

/* ------------------------------------------------------------------ */
/* Mail app: Configuration / Logs                                      */
/* ------------------------------------------------------------------ */

func (s *Server) handleAdminMailPage(w http.ResponseWriter, r *http.Request) {
	m := s.settings.Current().Mail
	s.renderPage(w, http.StatusOK, "admin-mail.gohtml", struct {
		adminPage
		Mail settings.Mail
	}{s.adminBase(r, "Mail app", "mail"), m})
}

func (s *Server) handleAdminMailSave(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	old := s.settings.Current().Mail
	m := old

	m.Enabled = r.FormValue("enabled") == "on"
	m.SMTPHost = strings.TrimSpace(r.FormValue("smtpHost"))
	m.SMTPPort = formInt(r, "smtpPort", old.SMTPPort)
	m.SMTPUsername = strings.TrimSpace(r.FormValue("smtpUsername"))
	m.SMTPFrom = strings.TrimSpace(r.FormValue("smtpFrom"))
	if v := r.FormValue("smtpPassword"); v != "" {
		m.SMTPPassword = v
	}
	m.IMAPHost = strings.TrimSpace(r.FormValue("imapHost"))
	m.IMAPPort = formInt(r, "imapPort", old.IMAPPort)
	m.IMAPUsername = strings.TrimSpace(r.FormValue("imapUsername"))
	if v := r.FormValue("imapPassword"); v != "" {
		m.IMAPPassword = v
	}

	var uid int64
	if user != nil {
		uid = user.ID
	}
	if err := s.settings.SaveGroup(r.Context(), settings.KeyMail, m, uid); err != nil {
		http.Redirect(w, r, "/admin/mail?err="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/mail?ok="+urlQuery("mail settings saved"), http.StatusSeeOther)
}

func (s *Server) handleAdminMailLogsPage(w http.ResponseWriter, r *http.Request) {
	s.renderAppCallsPage(w, r, "mail", "Mail app logs")
}

/* ------------------------------------------------------------------ */
/* HTTP app: Configuration / Logs                                      */
/* ------------------------------------------------------------------ */

func (s *Server) handleAdminHTTPPage(w http.ResponseWriter, r *http.Request) {
	h := s.settings.Current().HTTP
	s.renderPage(w, http.StatusOK, "admin-http.gohtml", struct {
		adminPage
		HTTP settings.HTTP
	}{s.adminBase(r, "HTTP app", "http"), h})
}

func (s *Server) handleAdminHTTPSave(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	old := s.settings.Current().HTTP
	h := old

	h.Enabled = r.FormValue("enabled") == "on"
	h.AllowedDomains = splitLines(r.FormValue("allowedDomains"))
	h.TimeoutSeconds = formInt(r, "timeoutSeconds", old.TimeoutSeconds)
	h.ResponseCapBytes = formInt(r, "responseCapBytes", old.ResponseCapBytes)

	var uid int64
	if user != nil {
		uid = user.ID
	}
	if err := s.settings.SaveGroup(r.Context(), settings.KeyHTTP, h, uid); err != nil {
		http.Redirect(w, r, "/admin/http?err="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/http?ok="+urlQuery("http settings saved"), http.StatusSeeOther)
}

func (s *Server) handleAdminHTTPLogsPage(w http.ResponseWriter, r *http.Request) {
	s.renderAppCallsPage(w, r, "http", "HTTP app logs")
}

func (s *Server) renderAppCallsPage(w http.ResponseWriter, r *http.Request, app, title string) {
	calls, err := s.store.ListAppCalls(r.Context(), store.AppCallFilter{App: app, Limit: 100})
	if err != nil {
		calls = []*store.AppCall{}
	}
	s.renderPage(w, http.StatusOK, "admin-appcalls.gohtml", struct {
		adminPage
		App   string
		Calls []*store.AppCall
	}{s.adminBase(r, title, app), app, calls})
}

/* ------------------------------------------------------------------ */
/* Logger app: Configuration / Logs (Loki)                             */
/* ------------------------------------------------------------------ */

func (s *Server) handleAdminLoggerPage(w http.ResponseWriter, r *http.Request) {
	l := s.settings.Current().Logger
	s.renderPage(w, http.StatusOK, "admin-logger.gohtml", struct {
		adminPage
		Logger settings.Logger
	}{s.adminBase(r, "Logger app", "logger"), l})
}

func (s *Server) handleAdminLoggerSave(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	old := s.settings.Current().Logger
	l := old

	l.Enabled = r.FormValue("enabled") == "on"
	l.PushURL = strings.TrimSpace(r.FormValue("pushUrl"))
	l.Username = strings.TrimSpace(r.FormValue("username"))
	if v := r.FormValue("password"); v != "" {
		l.Password = v
	}
	l.Tenant = strings.TrimSpace(r.FormValue("tenant"))
	l.TimeoutSeconds = formInt(r, "timeoutSeconds", old.TimeoutSeconds)

	var uid int64
	if user != nil {
		uid = user.ID
	}
	if err := s.settings.SaveGroup(r.Context(), settings.KeyLogger, l, uid); err != nil {
		http.Redirect(w, r, "/admin/logger?err="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/logger?ok="+urlQuery("logger settings saved"), http.StatusSeeOther)
}

// lokiLine is one decoded log line from a query_range response.
type lokiLine struct {
	Timestamp string
	Labels    string
	Line      string
}

func (s *Server) handleAdminLoggerLogsPage(w http.ResponseWriter, r *http.Request) {
	l := s.settings.Current().Logger
	lines := []lokiLine{}
	errMsg := ""

	if l.PushURL == "" {
		errMsg = "logger is not configured — set a Loki push URL first"
	} else {
		lines, errMsg = s.queryLoki(r.Context(), l, r.URL.Query().Get("user"))
	}

	s.renderPage(w, http.StatusOK, "admin-lokilogs.gohtml", struct {
		adminPage
		Logger settings.Logger
		Lines  []lokiLine
		Error  string
	}{s.adminBase(r, "Logger app logs", "logger"), l, lines, errMsg})
}

// queryLoki reads recent Logger.log entries straight from Loki's query_range
// API. The selector keeps labels low-cardinality: app + optional user.
func (s *Server) queryLoki(ctx context.Context, l settings.Logger, user string) ([]lokiLine, string) {
	selector := `{app="Logger.log"}`
	if user != "" {
		selector = fmt.Sprintf(`{app="Logger.log",user=%q}`, labelSafeUser(user))
	}
	base := strings.TrimSuffix(l.PushURL, "/")
	base = strings.TrimSuffix(base, "/loki/api/v1/push")
	q := url.Values{}
	q.Set("query", selector)
	q.Set("limit", "100")
	q.Set("start", strconv.FormatInt(time.Now().Add(-2*time.Hour).UnixNano(), 10))
	q.Set("end", strconv.FormatInt(time.Now().UnixNano(), 10))
	q.Set("direction", "backward")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/loki/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, err.Error()
	}
	if l.Tenant != "" {
		req.Header.Set("X-Scope-OrgID", l.Tenant)
	}
	if l.Username != "" {
		req.SetBasicAuth(l.Username, l.Password)
	}

	timeout := l.TimeoutSeconds
	if timeout <= 0 {
		timeout = 5
	}
	hc := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "loki query failed: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Sprintf("loki query returned HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Stream map[string]string `json:"stream"`
				Values [][2]string       `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Status != "success" {
		return nil, "loki returned an unparseable response"
	}

	out := []lokiLine{}
	for _, stream := range parsed.Data.Result {
		labels := fmt.Sprintf("user=%s project=%s", stream.Stream["user"], stream.Stream["project"])
		for _, v := range stream.Values {
			ts := v[0]
			if len(ts) >= 19 {
				if n, err := strconv.ParseInt(ts[:19], 10, 64); err == nil {
					ts = time.Unix(n, 0).UTC().Format(time.RFC3339)
				}
			}
			out = append(out, lokiLine{Timestamp: ts, Labels: labels, Line: v[1]})
		}
	}
	return out, ""
}

func labelSafeUser(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

/* ------------------------------------------------------------------ */
/* Rate limits (per app, per user)                                     */
/* ------------------------------------------------------------------ */

func (s *Server) handleAdminQuotasPage(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("app")
	if app == "" {
		app = "mail"
	}
	q := settings.QuotasKeyFor(app, s.settings.Current().Quotas)

	users, _ := s.store.ListUsers(r.Context())
	quotas, _ := s.store.ListAppQuotas(r.Context(), app)

	type row struct {
		User  *store.User
		Quota *store.AppQuota
	}
	rows := []row{}
	for _, u := range users {
		var rq *store.AppQuota
		for _, qq := range quotas {
			if qq.UserID == u.ID {
				rq = qq
				break
			}
		}
		rows = append(rows, row{User: u, Quota: rq})
	}

	s.renderPage(w, http.StatusOK, "admin-quotas.gohtml", struct {
		adminPage
		App     string
		Default int
		Rows    []row
	}{s.adminBase(r, "Rate limits", app), app, q, rows})
}

func (s *Server) handleAdminQuotaSave(w http.ResponseWriter, r *http.Request) {
	userID, err := strconv.ParseInt(r.FormValue("userId"), 10, 64)
	app := r.FormValue("app")
	if err != nil || userID <= 0 || app == "" {
		http.Redirect(w, r, "/admin/quotas?app="+urlQuery(app)+"&err="+urlQuery("bad user"), http.StatusSeeOther)
		return
	}
	q := store.AppQuota{UserID: userID, App: app, Enabled: r.FormValue("enabled") == "on"}
	if v := strings.TrimSpace(r.FormValue("callsPerMinute")); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n >= 0 {
			q.CallsPerMinute = &n
		}
	}
	if err := s.store.UpsertAppQuota(r.Context(), q); err != nil {
		http.Redirect(w, r, "/admin/quotas?app="+urlQuery(app)+"&err="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/quotas?app="+urlQuery(app)+"&ok="+urlQuery("quota saved"), http.StatusSeeOther)
}

func (s *Server) handleAdminQuotaDelete(w http.ResponseWriter, r *http.Request) {
	userID, err := strconv.ParseInt(r.FormValue("userId"), 10, 64)
	app := r.FormValue("app")
	if err != nil || app == "" {
		http.Redirect(w, r, "/admin/quotas?app="+urlQuery(app)+"&err="+urlQuery("bad user"), http.StatusSeeOther)
		return
	}
	if err := s.store.DeleteAppQuota(r.Context(), userID, app); err != nil {
		http.Redirect(w, r, "/admin/quotas?app="+urlQuery(app)+"&err="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/quotas?app="+urlQuery(app)+"&ok="+urlQuery("quota removed"), http.StatusSeeOther)
}

/* ------------------------------------------------------------------ */
/* System: Users / Executions / General                                */
/* ------------------------------------------------------------------ */

func (s *Server) handleAdminUsersPage(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		users = []*store.User{}
	}
	s.renderPage(w, http.StatusOK, "admin-users.gohtml", struct {
		adminPage
		Users []*store.User
	}{s.adminBase(r, "Users", "system"), users})
}

func (s *Server) handleAdminExecutionsPage(w http.ResponseWriter, r *http.Request) {
	audit, err := s.store.ListWSAudit(r.Context(), 100)
	if err != nil {
		audit = []*store.AuditEntry{}
	}
	s.renderPage(w, http.StatusOK, "admin-executions.gohtml", struct {
		adminPage
		Audit []*store.AuditEntry
	}{s.adminBase(r, "Executions", "system"), audit})
}

func (s *Server) handleAdminGeneralPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, http.StatusOK, "admin-general.gohtml", struct {
		adminPage
		Run settings.Run
	}{s.adminBase(r, "General", "system"), s.settings.Current().Run})
}

func (s *Server) handleAdminGeneralSave(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	old := s.settings.Current().Run
	run := old
	run.TimeoutSeconds = formInt(r, "timeoutSeconds", old.TimeoutSeconds)
	run.MaxConcurrent = formInt(r, "maxConcurrent", old.MaxConcurrent)
	run.RatePerMinute = formInt(r, "ratePerMinute", old.RatePerMinute)
	run.RateBurst = formInt(r, "rateBurst", old.RateBurst)
	run.DeployRatePerMinute = formInt(r, "deployRatePerMinute", old.DeployRatePerMinute)
	run.DeployRateBurst = formInt(r, "deployRateBurst", old.DeployRateBurst)
	run.OutputCapBytes = formInt(r, "outputCapBytes", old.OutputCapBytes)

	var uid int64
	if user != nil {
		uid = user.ID
	}
	if err := s.settings.SaveGroup(r.Context(), settings.KeyRun, run, uid); err != nil {
		http.Redirect(w, r, "/admin/general?err="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/general?ok="+urlQuery("saved — sandbox limits apply after a restart"), http.StatusSeeOther)
}

func (s *Server) handleAdminRotateServiceToken(w http.ResponseWriter, r *http.Request) {
	_ = s.currentUser(r) // requireAdminPage already verified access
	tok, err := s.settings.RotateInternal(r.Context(), settings.KeyServiceToken, 24)
	if err != nil {
		http.Redirect(w, r, "/admin/general?err="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	// Shown once: there is no way to read it back later.
	http.Redirect(w, r, "/admin/general?ok="+urlQuery("new service token: "+tok), http.StatusSeeOther)
}

/* ------------------------------------------------------------------ */

func formInt(r *http.Request, key string, fallback int) int {
	if v := strings.TrimSpace(r.FormValue(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func splitLines(s string) []string {
	out := []string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
