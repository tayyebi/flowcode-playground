package main

// The server-rendered administration pages. Same conventions as the user
// area: no JavaScript, Go templates, plain form POSTs, flash messages on the
// redirect's query string. Static assets come from nginx, not this binary.

import (
	"embed"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/tayyebi/flowcode-playground/server/internal/settings"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

//go:embed templates/*.gohtml
var adminFS embed.FS

var pageTemplates = template.Must(template.ParseFS(adminFS, "templates/*.gohtml"))

// basePage carries what every page's chrome needs.
type basePage struct {
	Title   string
	Flash   string
	FlashOK bool
}

// adminPage adds the sidebar highlight section.
type adminPage struct {
	basePage
	Section string // "", "mail", "http", "logger", "system"
}

func (s *Server) adminBase(r *http.Request, title, section string) adminPage {
	b := adminPage{basePage: basePage{Title: title}, Section: section}
	if v := r.URL.Query().Get("ok"); v != "" {
		b.Flash, b.FlashOK = v, true
	}
	if v := r.URL.Query().Get("err"); v != "" {
		b.Flash = v
	}
	return b
}

func (s *Server) renderPage(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pageTemplates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, http.StatusOK, "login.gohtml", s.adminBase(r, "Log in", ""))
}

/* ------------------------------------------------------------------ */
/* Settings saves + app reload notification                            */
/* ------------------------------------------------------------------ */

// saveGroup persists a settings group and pokes the app's internal reload
// endpoint so the user-facing binary picks the change up immediately. The
// database write is the source of truth; the poke is best-effort (the app
// also refreshes every minute on its own), so its failure is only a note.
func (s *Server) saveGroup(w http.ResponseWriter, r *http.Request, key string, group any, back, okMsg string) {
	if err := s.settings.SaveGroup(r.Context(), key, group, 0); err != nil {
		http.Redirect(w, r, back+"?err="+queryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	msg := okMsg
	if err := s.notifyAppReload(r); err != nil {
		msg += " (app will pick it up within a minute)"
	}
	http.Redirect(w, r, back+"?ok="+queryEscape(msg), http.StatusSeeOther)
}

func (s *Server) notifyAppReload(r *http.Request) error {
	if s.internalURL == "" {
		return nil
	}
	tok, err := s.store.GetSetting(r.Context(), settings.KeyServiceToken)
	if err != nil || tok == "" {
		return err
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.internalURL+"/internal/reload", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return errUnexpectedStatus(resp.StatusCode)
	}
	return nil
}

/* ------------------------------------------------------------------ */
/* Home                                                                */
/* ------------------------------------------------------------------ */

func (s *Server) handleAdminHomePage(w http.ResponseWriter, r *http.Request) {
	snap := s.settings.Current()
	adminCount, _ := s.store.CountAdministrators(r.Context())
	s.renderPage(w, http.StatusOK, "home.gohtml", struct {
		adminPage
		Mail, HTTP, Logger bool
		Quotas             settings.Quotas
		AdminCount         int
	}{s.adminBase(r, "Administration", ""), snap.Mail.Enabled, snap.HTTP.Enabled, snap.Logger.Enabled, snap.Quotas, adminCount})
}

/* ------------------------------------------------------------------ */
/* Mail app: Configuration / Logs                                      */
/* ------------------------------------------------------------------ */

func (s *Server) handleAdminMailPage(w http.ResponseWriter, r *http.Request) {
	m := s.settings.Current().Mail
	s.renderPage(w, http.StatusOK, "mail.gohtml", struct {
		adminPage
		Mail settings.Mail
	}{s.adminBase(r, "Mail app", "mail"), m})
}

func (s *Server) handleAdminMailSave(w http.ResponseWriter, r *http.Request) {
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
	// TLS is the checkbox semantics inverted by hand: unchecked means the
	// admin wants plaintext IMAP (the local rig); absent field on an old
	// saved group keeps the true default until the form is saved.
	m.IMAPTLS = r.FormValue("imapTLS") == "on"
	m.IMAPUsername = strings.TrimSpace(r.FormValue("imapUsername"))
	if v := r.FormValue("imapPassword"); v != "" {
		m.IMAPPassword = v
	}

	s.saveGroup(w, r, settings.KeyMail, m, "/admin/mail", "mail settings saved")
}

func (s *Server) handleAdminMailLogsPage(w http.ResponseWriter, r *http.Request) {
	s.renderAppCallsPage(w, r, "mail", "Mail app logs")
}

/* ------------------------------------------------------------------ */
/* HTTP app: Configuration / Logs                                      */
/* ------------------------------------------------------------------ */

func (s *Server) handleAdminHTTPPage(w http.ResponseWriter, r *http.Request) {
	h := s.settings.Current().HTTP
	s.renderPage(w, http.StatusOK, "http.gohtml", struct {
		adminPage
		HTTP settings.HTTP
	}{s.adminBase(r, "HTTP app", "http"), h})
}

func (s *Server) handleAdminHTTPSave(w http.ResponseWriter, r *http.Request) {
	old := s.settings.Current().HTTP
	h := old

	h.Enabled = r.FormValue("enabled") == "on"
	h.AllowedDomains = splitLines(r.FormValue("allowedDomains"))
	h.TimeoutSeconds = formInt(r, "timeoutSeconds", old.TimeoutSeconds)
	h.ResponseCapBytes = formInt(r, "responseCapBytes", old.ResponseCapBytes)

	s.saveGroup(w, r, settings.KeyHTTP, h, "/admin/http", "http settings saved")
}

func (s *Server) handleAdminHTTPLogsPage(w http.ResponseWriter, r *http.Request) {
	s.renderAppCallsPage(w, r, "http", "HTTP app logs")
}

func (s *Server) renderAppCallsPage(w http.ResponseWriter, r *http.Request, app, title string) {
	calls, err := s.store.ListAppCalls(r.Context(), store.AppCallFilter{App: app, Limit: 100})
	if err != nil {
		calls = []*store.AppCall{}
	}
	s.renderPage(w, http.StatusOK, "appcalls.gohtml", struct {
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
	s.renderPage(w, http.StatusOK, "logger.gohtml", struct {
		adminPage
		Logger settings.Logger
	}{s.adminBase(r, "Logger app", "logger"), l})
}

func (s *Server) handleAdminLoggerSave(w http.ResponseWriter, r *http.Request) {
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

	s.saveGroup(w, r, settings.KeyLogger, l, "/admin/logger", "logger settings saved")
}

func (s *Server) handleAdminLoggerLogsPage(w http.ResponseWriter, r *http.Request) {
	lines, errMsg := s.queryLokiLines(r)
	s.renderPage(w, http.StatusOK, "lokilogs.gohtml", struct {
		adminPage
		Lines []lokiLine
		Error string
	}{s.adminBase(r, "Logger app logs", "logger"), lines, errMsg})
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

	s.renderPage(w, http.StatusOK, "quotas.gohtml", struct {
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
		http.Redirect(w, r, "/admin/quotas?app="+queryEscape(app)+"&err="+queryEscape("bad user"), http.StatusSeeOther)
		return
	}
	q := store.AppQuota{UserID: userID, App: app, Enabled: r.FormValue("enabled") == "on"}
	if v := strings.TrimSpace(r.FormValue("callsPerMinute")); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n >= 0 {
			q.CallsPerMinute = &n
		}
	}
	if err := s.store.UpsertAppQuota(r.Context(), q); err != nil {
		http.Redirect(w, r, "/admin/quotas?app="+queryEscape(app)+"&err="+queryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	// Quota overrides are read per call by the app; no reload poke needed.
	http.Redirect(w, r, "/admin/quotas?app="+queryEscape(app)+"&ok="+queryEscape("quota saved"), http.StatusSeeOther)
}

func (s *Server) handleAdminQuotaDelete(w http.ResponseWriter, r *http.Request) {
	userID, err := strconv.ParseInt(r.FormValue("userId"), 10, 64)
	app := r.FormValue("app")
	if err != nil || app == "" {
		http.Redirect(w, r, "/admin/quotas?app="+queryEscape(app)+"&err="+queryEscape("bad user"), http.StatusSeeOther)
		return
	}
	if err := s.store.DeleteAppQuota(r.Context(), userID, app); err != nil {
		http.Redirect(w, r, "/admin/quotas?app="+queryEscape(app)+"&err="+queryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/quotas?app="+queryEscape(app)+"&ok="+queryEscape("quota removed"), http.StatusSeeOther)
}

/* ------------------------------------------------------------------ */
/* System: Users / Executions / General / Administrators               */
/* ------------------------------------------------------------------ */

func (s *Server) handleAdminUsersPage(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		users = []*store.User{}
	}
	s.renderPage(w, http.StatusOK, "users.gohtml", struct {
		adminPage
		Users []*store.User
	}{s.adminBase(r, "Users", "system"), users})
}

func (s *Server) handleAdminExecutionsPage(w http.ResponseWriter, r *http.Request) {
	audit, err := s.store.ListWSAudit(r.Context(), 100)
	if err != nil {
		audit = []*store.AuditEntry{}
	}
	s.renderPage(w, http.StatusOK, "executions.gohtml", struct {
		adminPage
		Audit []*store.AuditEntry
	}{s.adminBase(r, "Executions", "system"), audit})
}

func (s *Server) handleAdminGeneralPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, http.StatusOK, "general.gohtml", struct {
		adminPage
		Run settings.Run
	}{s.adminBase(r, "General", "system"), s.settings.Current().Run})
}

func (s *Server) handleAdminGeneralSave(w http.ResponseWriter, r *http.Request) {
	old := s.settings.Current().Run
	run := old
	run.TimeoutSeconds = formInt(r, "timeoutSeconds", old.TimeoutSeconds)
	run.MaxConcurrent = formInt(r, "maxConcurrent", old.MaxConcurrent)
	run.RatePerMinute = formInt(r, "ratePerMinute", old.RatePerMinute)
	run.RateBurst = formInt(r, "rateBurst", old.RateBurst)
	run.DeployRatePerMinute = formInt(r, "deployRatePerMinute", old.DeployRatePerMinute)
	run.DeployRateBurst = formInt(r, "deployRateBurst", old.DeployRateBurst)
	run.OutputCapBytes = formInt(r, "outputCapBytes", old.OutputCapBytes)

	s.saveGroup(w, r, settings.KeyRun, run, "/admin/general", "saved — sandbox limits apply after the app restarts")
}

func (s *Server) handleAdminRotateServiceToken(w http.ResponseWriter, r *http.Request) {
	tok, err := s.settings.RotateInternal(r.Context(), settings.KeyServiceToken, 24)
	if err != nil {
		http.Redirect(w, r, "/admin/general?err="+queryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	msg := "new service token: " + tok
	if err := s.notifyAppReloadWithToken(r, tok); err != nil {
		msg += " (the app refreshes its copy within a minute)"
	}
	// Shown once: there is no way to read it back later.
	http.Redirect(w, r, "/admin/general?ok="+queryEscape(msg), http.StatusSeeOther)
}

// notifyAppReloadWithToken is the rotate variant: the app still holds the
// old token in memory, so the poke authenticates with the fresh one, which
// the reload endpoint reads straight from the database.
func (s *Server) notifyAppReloadWithToken(r *http.Request, tok string) error {
	if s.internalURL == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.internalURL+"/internal/reload", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return errUnexpectedStatus(resp.StatusCode)
	}
	return nil
}

/* ------------------------------------------------------------------ */
/* Administrators: the allowlist itself                                */
/* ------------------------------------------------------------------ */

func (s *Server) handleAdminAdministratorsPage(w http.ResponseWriter, r *http.Request) {
	admins, err := s.store.ListAdministrators(r.Context())
	if err != nil {
		admins = []*store.Administrator{}
	}
	s.renderPage(w, http.StatusOK, "administrators.gohtml", struct {
		adminPage
		Admins         []*store.Administrator
		CurrentAdminID int64
	}{s.adminBase(r, "Administrators", "system"), admins, s.currentAdministrator(r).ID})
}

func (s *Server) handleAdminAdministratorAdd(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.FormValue("email"))
	if email == "" || !strings.Contains(email, "@") {
		http.Redirect(w, r, "/admin/administrators?err="+queryEscape("an email address is required"), http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if _, err := s.store.AddAdministrator(r.Context(), email, name); err != nil {
		http.Redirect(w, r, "/admin/administrators?err="+queryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/administrators?ok="+queryEscape(email+" allowlisted — they can sign in on their next login"), http.StatusSeeOther)
}

func (s *Server) handleAdminAdministratorRemove(w http.ResponseWriter, r *http.Request) {
	me := s.currentAdministrator(r)
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, "/admin/administrators?err="+queryEscape("bad administrator"), http.StatusSeeOther)
		return
	}
	if me != nil && me.ID == id {
		http.Redirect(w, r, "/admin/administrators?err="+queryEscape("you cannot remove yourself"), http.StatusSeeOther)
		return
	}
	if n, _ := s.store.CountAdministrators(r.Context()); n <= 1 {
		http.Redirect(w, r, "/admin/administrators?err="+queryEscape("the last administrator cannot be removed"), http.StatusSeeOther)
		return
	}
	if err := s.store.RemoveAdministrator(r.Context(), id); err != nil {
		http.Redirect(w, r, "/admin/administrators?err="+queryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/administrators?ok="+queryEscape("administrator removed"), http.StatusSeeOther)
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

type statusError int

func (e statusError) Error() string { return "unexpected HTTP status " + strconv.Itoa(int(e)) }

func errUnexpectedStatus(code int) error { return statusError(code) }
