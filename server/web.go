package main

// The server-rendered UI. There is no JavaScript anywhere: every page is a
// Go template executed against store data, and every action is a plain HTML
// form POST that redirects (or, for Run, renders the result in the response).

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/engine"
	"github.com/tayyebi/flowcode-playground/server/internal/scheduler"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

//go:embed web/templates/*.gohtml web/static/style.css
var webFS embed.FS

var pageTemplates = template.Must(template.ParseFS(webFS, "web/templates/*.gohtml"))

// newFileTemplate is what a freshly added file starts from — the same
// starting point the SPA used to seed its editor with.
const newFileTemplate = `workflow: NewWorkflow

step first:
    emit
        value = "hello"
end
`

// basePage carries what every page's chrome needs. Flash messages ride the
// redirect's query string (?ok=… / ?err=…), which keeps POST handlers simple
// and the pages linkable.
type basePage struct {
	Title        string
	Flash        string
	FlashOK      bool
	AdminEnabled bool // the server has a PLAYGROUND_ADMIN_TOKEN configured
}

func (s *Server) basePage(r *http.Request, title string) basePage {
	b := basePage{Title: title, AdminEnabled: s.AdminToken != ""}
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

// requireAdminPage is requireAdmin for HTML routes: bounce to /login instead
// of writing a JSON 401.
func (s *Server) requireAdminPage(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.AdminToken == "" || s.adminTokenMatches(adminTokenFromRequest(r)) {
			next(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

func handleStaticCSS(w http.ResponseWriter, r *http.Request) {
	b, err := webFS.ReadFile("web/static/style.css")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(b)
}

/* ------------------------------------------------------------------ */
/* Entry, login                                                        */
/* ------------------------------------------------------------------ */

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

type loginPage struct {
	basePage
	BadToken bool
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.AdminToken == "" {
		http.Redirect(w, r, "/projects", http.StatusSeeOther)
		return
	}
	s.renderPage(w, http.StatusOK, "login.gohtml", loginPage{basePage: s.basePage(r, "Log in")})
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if s.AdminToken == "" {
		http.Redirect(w, r, "/projects", http.StatusSeeOther)
		return
	}
	token := r.FormValue("token")
	if !s.adminTokenMatches(token) {
		s.renderPage(w, http.StatusUnauthorized, "login.gohtml",
			loginPage{basePage: s.basePage(r, "Log in"), BadToken: true})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminCookieName, Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: 30 * 24 * 60 * 60,
	})
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

func (s *Server) handleLogoutPost(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: adminCookieName, Value: "", Path: "/", MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

/* ------------------------------------------------------------------ */
/* Projects list                                                       */
/* ------------------------------------------------------------------ */

type projectsPage struct {
	basePage
	Projects []*store.Project
}

func (s *Server) handleProjectsPage(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		s.renderPage(w, http.StatusInternalServerError, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Error"), Message: "could not list projects"})
		return
	}
	s.renderPage(w, http.StatusOK, "projects.gohtml",
		projectsPage{basePage: s.basePage(r, "Projects"), Projects: projects})
}

func (s *Server) handleProjectCreatePage(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, "/projects?err="+urlQuery("a project needs a name"), http.StatusSeeOther)
		return
	}
	p, err := s.store.CreateProject(r.Context(), name, r.FormValue("description"))
	if err != nil {
		http.Redirect(w, r, "/projects?err="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d?ok=%s", p.ID, urlQuery("project created")), http.StatusSeeOther)
}

func (s *Server) handleProjectDeletePage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteProject(r.Context(), p.ID); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("could not delete project")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects?ok="+urlQuery("project deleted"), http.StatusSeeOther)
}

func (s *Server) handleProjectUpdatePage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("a project needs a name")), http.StatusSeeOther)
		return
	}
	// UpdateProject takes nil to leave a field alone.
	newName, newDesc := name, r.FormValue("description")
	if _, err := s.store.UpdateProject(r.Context(), p.ID, &newName, &newDesc); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery(err.Error())), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d?ok=%s", p.ID, urlQuery("saved")), http.StatusSeeOther)
}

/* ------------------------------------------------------------------ */
/* Project workspace                                                   */
/* ------------------------------------------------------------------ */

type errorPage struct {
	basePage
	Message string
}

type projectPage struct {
	basePage
	P           *store.Project
	Files       []*store.File
	Selected    *store.File
	Result      *runResult // non-nil only in the response to a Run POST
	Versions    []*store.Version
	Deployments []*store.Deployment
	Triggers    []*store.Trigger
	Executions  []*store.Execution
	KV          []*store.KVEntry
}

// projectOrNotFoundPage is getProjectOr404 for HTML routes.
func (s *Server) projectOrNotFoundPage(w http.ResponseWriter, r *http.Request) (*store.Project, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return nil, false
	}
	p, err := s.store.GetProject(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.renderPage(w, http.StatusNotFound, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Not found"), Message: "project not found"})
		return nil, false
	}
	if err != nil {
		s.renderPage(w, http.StatusInternalServerError, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Error"), Message: "could not look up project"})
		return nil, false
	}
	return p, true
}

// projectPageData assembles everything the workspace template shows.
func (s *Server) projectPageData(r *http.Request, p *store.Project, result *runResult) (*projectPage, error) {
	page := &projectPage{basePage: s.basePage(r, p.Name), P: p, Result: result}
	var err error
	if page.Files, err = s.store.ListFiles(r.Context(), p.ID); err != nil {
		return nil, err
	}
	if page.Versions, err = s.store.ListVersions(r.Context(), p.ID); err != nil {
		return nil, err
	}
	if page.Deployments, err = s.store.ListDeployments(r.Context(), p.ID); err != nil {
		return nil, err
	}
	if page.Triggers, err = s.store.ListTriggers(r.Context(), p.ID); err != nil {
		return nil, err
	}
	if page.Executions, err = s.store.ListExecutions(r.Context(), p.ID, 20, 0); err != nil {
		return nil, err
	}
	if page.KV, err = s.store.ListKV(r.Context(), p.ID); err != nil {
		return nil, err
	}

	if name := r.URL.Query().Get("file"); name != "" {
		for _, f := range page.Files {
			if f.Name == name {
				page.Selected = f
				break
			}
		}
	}
	if page.Selected == nil && len(page.Files) > 0 {
		page.Selected = page.Files[0]
	}
	return page, nil
}

func (s *Server) renderProjectPage(w http.ResponseWriter, r *http.Request, status int, p *store.Project, result *runResult) {
	page, err := s.projectPageData(r, p, result)
	if err != nil {
		s.renderPage(w, http.StatusInternalServerError, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Error"), Message: "could not load project"})
		return
	}
	s.renderPage(w, status, "project.gohtml", page)
}

func (s *Server) handleProjectPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	s.renderProjectPage(w, r, http.StatusOK, p, nil)
}

/* ------------------------------------------------------------------ */
/* Files                                                               */
/* ------------------------------------------------------------------ */

// sanitizeFileName enforces the one convention the whole product assumes:
// a file is a name ending in .fc, with no path in it.
func sanitizeFileName(raw string) string {
	name := strings.TrimSpace(raw)
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if name != "" && !strings.HasSuffix(name, ".fc") {
		name += ".fc"
	}
	return name
}

func (s *Server) handleNewFilePage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	name := sanitizeFileName(r.FormValue("name"))
	back := fmt.Sprintf("/projects/%d", p.ID)
	if name == "" {
		http.Redirect(w, r, back+"?err="+urlQuery("a file needs a name"), http.StatusSeeOther)
		return
	}
	if _, err := s.store.UpsertFile(r.Context(), p.ID, name, newFileTemplate); err != nil {
		http.Redirect(w, r, back+"?err="+urlQuery("could not create file"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, back+"?file="+urlQuery(name)+"&ok="+urlQuery("file created"), http.StatusSeeOther)
}

func (s *Server) handleSaveFilePage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	content := r.FormValue("content")
	back := fmt.Sprintf("/projects/%d?file=%s", p.ID, urlQuery(name))
	if len(content) > maxSourceBytes {
		http.Redirect(w, r, back+"&err="+urlQuery("file too large"), http.StatusSeeOther)
		return
	}
	if _, err := s.store.UpsertFile(r.Context(), p.ID, name, content); err != nil {
		http.Redirect(w, r, back+"&err="+urlQuery("could not save file"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, back+"&ok="+urlQuery("file saved"), http.StatusSeeOther)
}

func (s *Server) handleDeleteFilePage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteFile(r.Context(), p.ID, r.PathValue("name")); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("could not delete file")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d?ok=%s", p.ID, urlQuery("file deleted")), http.StatusSeeOther)
}

// handleRunFilePage runs the file and renders the workspace with the full
// result in place — the one POST that answers with a page rather than a
// redirect, because the fresh result (bytecode included) exists only in
// this response. Refreshing will re-POST, which re-runs; the execution is
// recorded either way.
func (s *Server) handleRunFilePage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	f, err := s.store.GetFile(r.Context(), p.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		s.renderPage(w, http.StatusNotFound, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Not found"), Message: "file not found"})
		return
	}
	if err != nil {
		s.renderPage(w, http.StatusInternalServerError, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Error"), Message: "could not look up file"})
		return
	}

	started := time.Now()
	result, runErr := s.engine.Run(r.Context(), f.Content)
	finished := time.Now()

	if runErr != nil && !errors.Is(runErr, engine.ErrBusy) && r.Context().Err() != nil {
		return // client went away; nothing to write back
	}
	exec, execErr := s.recordExecution(r.Context(), p.ID, f, nil, "project-run", nil, nil, started, finished, result, runErr)
	if execErr != nil {
		s.renderPage(w, http.StatusInternalServerError, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Error"), Message: "could not record execution"})
		return
	}
	if runErr != nil {
		s.renderPage(w, http.StatusServiceUnavailable, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Busy"), Message: "the engine is busy — try again shortly"})
		return
	}
	s.renderProjectPage(w, r, http.StatusOK, p, &runResult{Result: result, ExecutionID: exec.ID})
}

/* ------------------------------------------------------------------ */
/* Versions, deployments, triggers, KV                                 */
/* ------------------------------------------------------------------ */

func (s *Server) handleCreateVersionPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if _, err := s.store.CreateVersion(r.Context(), p.ID, r.FormValue("label")); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery(err.Error())), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d?ok=%s", p.ID, urlQuery("version saved")), http.StatusSeeOther)
}

func (s *Server) handleRestoreVersionPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("bad version number")), http.StatusSeeOther)
		return
	}
	v, err := s.store.GetVersionByNumber(r.Context(), p.ID, number)
	if err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("version not found")), http.StatusSeeOther)
		return
	}
	if err := s.store.RestoreVersion(r.Context(), p.ID, v.ID); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("could not restore version")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d?ok=%s", p.ID, urlQuery("version restored")), http.StatusSeeOther)
}

func (s *Server) handleCreateDeploymentPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	f, err := s.store.GetFile(r.Context(), p.ID, r.FormValue("fileName"))
	if errors.Is(err, store.ErrNotFound) {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("file not found")), http.StatusSeeOther)
		return
	}
	if err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("could not look up file")), http.StatusSeeOther)
		return
	}
	if _, err := s.store.CreateDeployment(r.Context(), p.ID, f.ID, nil); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery(err.Error())), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d?ok=%s", p.ID, urlQuery("deployment created")), http.StatusSeeOther)
}

func (s *Server) handleToggleDeploymentPage(w http.ResponseWriter, r *http.Request) {
	s.mutateDeployment(w, r, func(depID int64, enabled bool) error {
		return s.store.SetDeploymentEnabled(r.Context(), depID, !enabled)
	})
}

func (s *Server) handleDeleteDeploymentPage(w http.ResponseWriter, r *http.Request) {
	s.mutateDeployment(w, r, func(depID int64, _ bool) error {
		return s.store.DeleteDeployment(r.Context(), depID)
	})
}

func (s *Server) mutateDeployment(w http.ResponseWriter, r *http.Request, fn func(depID int64, enabled bool) error) {
	p, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	depID, err := strconv.ParseInt(r.PathValue("depId"), 10, 64)
	if err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("bad deployment id")), http.StatusSeeOther)
		return
	}
	dep, err := s.store.GetDeployment(r.Context(), depID)
	if err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("deployment not found")), http.StatusSeeOther)
		return
	}
	if err := fn(depID, dep.Enabled); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("could not update deployment")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d?ok=%s", p.ID, urlQuery("saved")), http.StatusSeeOther)
}

func (s *Server) handleCreateTriggerPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	backErr := func(msg string) {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery(msg)), http.StatusSeeOther)
	}
	f, err := s.store.GetFile(r.Context(), p.ID, r.FormValue("fileName"))
	if errors.Is(err, store.ErrNotFound) {
		backErr("file not found")
		return
	}
	if err != nil {
		backErr("could not look up file")
		return
	}

	nt := store.NewTrigger{
		ProjectID:    p.ID,
		FileID:       f.ID,
		ScheduleType: r.FormValue("scheduleType"),
	}
	switch nt.ScheduleType {
	case "interval":
		n, err := strconv.Atoi(r.FormValue("intervalSeconds"))
		if err != nil || n < 1 {
			backErr("interval triggers need a positive interval in seconds")
			return
		}
		nt.IntervalSeconds = &n
	case "daily":
		nt.DailyTimeUTC = strings.TrimSpace(r.FormValue("dailyTimeUTC"))
		if nt.DailyTimeUTC == "" {
			backErr("daily triggers need a time (UTC, HH:MM)")
			return
		}
	default:
		backErr("schedule type must be interval or daily")
		return
	}

	// Same validation the JSON API goes through: NextRun both checks the
	// schedule fields and produces the first fire time.
	next, err := scheduler.NextRun(time.Now(), nt.ScheduleType, *nt.IntervalSeconds, nt.DailyTimeUTC)
	if err != nil {
		backErr(err.Error())
		return
	}
	nt.NextRunAt = scheduler.FormatTime(next)

	if _, err := s.store.CreateTrigger(r.Context(), nt); err != nil {
		backErr(err.Error())
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d?ok=%s", p.ID, urlQuery("trigger created")), http.StatusSeeOther)
}

func (s *Server) handleToggleTriggerPage(w http.ResponseWriter, r *http.Request) {
	s.mutateTrigger(w, r, func(trigID int64, enabled bool) error {
		return s.store.SetTriggerEnabled(r.Context(), trigID, !enabled)
	})
}

func (s *Server) handleDeleteTriggerPage(w http.ResponseWriter, r *http.Request) {
	s.mutateTrigger(w, r, func(trigID int64, _ bool) error {
		return s.store.DeleteTrigger(r.Context(), trigID)
	})
}

func (s *Server) mutateTrigger(w http.ResponseWriter, r *http.Request, fn func(trigID int64, enabled bool) error) {
	p, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	trigID, err := strconv.ParseInt(r.PathValue("trigId"), 10, 64)
	if err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("bad trigger id")), http.StatusSeeOther)
		return
	}
	t, err := s.store.GetTrigger(r.Context(), trigID)
	if err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("trigger not found")), http.StatusSeeOther)
		return
	}
	if err := fn(trigID, t.Enabled); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("could not update trigger")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d?ok=%s", p.ID, urlQuery("saved")), http.StatusSeeOther)
}

func (s *Server) handleDeleteKVPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteKV(r.Context(), p.ID, r.PathValue("key")); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("could not delete kv entry")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d?ok=%s", p.ID, urlQuery("kv entry deleted")), http.StatusSeeOther)
}

/* ------------------------------------------------------------------ */
/* Execution detail                                                    */
/* ------------------------------------------------------------------ */

type executionPage struct {
	basePage
	P    *store.Project
	Exec *store.Execution
}

func (s *Server) handleExecutionPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	execID, err := strconv.ParseInt(r.PathValue("execId"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	exec, err := s.store.GetExecution(r.Context(), execID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && exec.ProjectID != p.ID) {
		s.renderPage(w, http.StatusNotFound, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Not found"), Message: "execution not found"})
		return
	}
	if err != nil {
		s.renderPage(w, http.StatusInternalServerError, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Error"), Message: "could not look up execution"})
		return
	}
	s.renderPage(w, http.StatusOK, "execution.gohtml",
		executionPage{basePage: s.basePage(r, "Execution #"+strconv.FormatInt(exec.ID, 10)), P: p, Exec: exec})
}

// urlQuery escapes a flash or file-name value for a query string.
func urlQuery(s string) string {
	return url.QueryEscape(s)
}
