package main

// The server-rendered UI. There is no JavaScript anywhere: every page is a
// Go template executed against store data, and every action is a plain HTML
// form POST that redirects (or, for Run, renders the result in the response).
//
// The project area is a three-rail shell: the projects switcher, the section
// nav (Overview / Files / Versions / …), and the section content. The Files
// section adds a third rail (the file list) beside a full-height editor whose
// top bar carries the file-name field plus Save and Run — one form, two
// submit targets, via the submit buttons' formaction attribute.

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/applogs"
	"github.com/tayyebi/flowcode-playground/server/internal/engine"
	"github.com/tayyebi/flowcode-playground/server/internal/scheduler"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

//go:embed web/templates/*.gohtml
var webFS embed.FS

var pageTemplates = template.Must(template.ParseFS(webFS, "web/templates/*.gohtml"))

// newFileTemplate is what a freshly added file starts from.
const newFileTemplate = `workflow: NewWorkflow

step first:
    emit
        value = "hello"
end
`

// basePage carries what every page's chrome needs. Flash messages ride the
// redirect's query string (?ok=… / ?err=…), which keeps POST handlers simple
// and the pages linkable. Rail, when set, opens the app shell (project
// switcher + section nav) instead of the bare centered page.
type basePage struct {
	Title   string
	Flash   string
	FlashOK bool
	User    *store.User
	Rail    *rail
}

// rail is the sidebar chrome shared by every app-shell page: the project
// switcher list, and — when a project is open — which project and section
// are active so the rails can highlight them.
type rail struct {
	Projects []*store.ProjectAccess
	ActiveID int64
	Project  *store.WSProject // nil on the projects home page
	Section  string           // "overview", "files", "versions", "logs", …
	Role     store.Role
	CanEdit  bool
}

func (s *Server) basePage(r *http.Request, title string) basePage {
	b := basePage{Title: title, User: s.currentUser(r)}
	if v := r.URL.Query().Get("ok"); v != "" {
		b.Flash, b.FlashOK = v, true
	}
	if v := r.URL.Query().Get("err"); v != "" {
		b.Flash = v
	}
	return b
}

// appPage opens the app shell: it loads the user's projects for the
// switcher rail and records the open project + active section.
func (s *Server) appPage(r *http.Request, title string, p *store.WSProject, role store.Role, section string) basePage {
	b := s.basePage(r, title)
	rd := &rail{Project: p, Section: section, Role: role, CanEdit: pageMayEdit(role)}
	if p != nil {
		rd.ActiveID = p.ID
	}
	if b.User != nil {
		if projects, err := s.store.ListProjectsForUser(r.Context(), b.User.ID); err == nil {
			rd.Projects = projects
		}
	}
	b.Rail = rd
	return b
}

func (s *Server) renderPage(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pageTemplates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
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
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, http.StatusOK, "login.gohtml", loginPage{basePage: s.basePage(r, "Log in")})
}

/* ------------------------------------------------------------------ */
/* Projects home + create/delete/update                                */
/* ------------------------------------------------------------------ */

// handleProjectsPage renders the app shell with no project open: the rails
// plus an empty-state main inviting the user to pick or create a project.
func (s *Server) handleProjectsPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, http.StatusOK, "projects.gohtml", s.appPage(r, "Projects", nil, "", ""))
}

func (s *Server) handleProjectCreatePage(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, "/projects?err="+urlQuery("a project needs a name"), http.StatusSeeOther)
		return
	}
	wsID, ok := s.userWorkspaceID(r, user)
	if !ok {
		http.Redirect(w, r, "/projects?err="+urlQuery("you have no workspace"), http.StatusSeeOther)
		return
	}
	p, err := s.store.CreateWSProject(r.Context(), wsID, user.ID, name, r.FormValue("description"))
	if err != nil {
		http.Redirect(w, r, "/projects?err="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d?ok=%s", p.ID, urlQuery("project created")), http.StatusSeeOther)
}

func (s *Server) handleProjectDeletePage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if role != store.RoleOwner && role != store.RoleAdmin {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("only the owner can delete a project")), http.StatusSeeOther)
		return
	}
	if err := s.store.DeleteWSProject(r.Context(), p.ID); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("could not delete project")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects?ok="+urlQuery("project deleted"), http.StatusSeeOther)
}

func (s *Server) handleProjectUpdatePage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if !pageMayEdit(role) {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("you have view-only access")), http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery("a project needs a name")), http.StatusSeeOther)
		return
	}
	newName, newDesc := name, r.FormValue("description")
	if _, err := s.store.UpdateWSProject(r.Context(), p.ID, &newName, &newDesc); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d?err=%s", p.ID, urlQuery(err.Error())), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d?ok=%s", p.ID, urlQuery("saved")), http.StatusSeeOther)
}

/* ------------------------------------------------------------------ */
/* Project shell helpers                                               */
/* ------------------------------------------------------------------ */

type errorPage struct {
	basePage
	Message string
}

// projectPage is the common payload of every per-project section page.
type projectPage struct {
	basePage
	P       *store.WSProject
	Role    store.Role
	CanEdit bool
}

func pageMayEdit(role store.Role) bool {
	return role == store.RoleEditor || role == store.RoleOwner || role == store.RoleAdmin
}

// projectOrNotFoundPage is the page-flavoured project resolver.
func (s *Server) projectOrNotFoundPage(w http.ResponseWriter, r *http.Request) (*store.WSProject, store.Role, *store.User, bool) {
	p, role, user, status, msg := s.resolveWSProject(r)
	if status != 0 {
		if status == http.StatusUnauthorized {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return nil, "", nil, false
		}
		s.renderPage(w, status, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Not found"), Message: msg})
		return nil, "", nil, false
	}
	return p, role, user, true
}

// newProjectPage starts a section page: shell rails plus the project basics.
func (s *Server) newProjectPage(r *http.Request, p *store.WSProject, role store.Role, section string) projectPage {
	return projectPage{
		basePage: s.appPage(r, p.Name+" · "+section, p, role, section),
		P:        p, Role: role, CanEdit: pageMayEdit(role),
	}
}

// filesBack is the editor URL with the given file selected.
func filesBack(pID int64, file string) string {
	return fmt.Sprintf("/projects/%d/files?file=%s", pID, urlQuery(file))
}

/* ------------------------------------------------------------------ */
/* Overview                                                            */
/* ------------------------------------------------------------------ */

type overviewPage struct {
	projectPage
	FileCount        int
	VersionCount     int
	DeploymentCount  int
	TriggerCount     int
	RecentExecutions []*store.Execution
}

func (s *Server) handleProjectPage(w http.ResponseWriter, r *http.Request) {
	// Old links selected a file via ?file= on the workspace page; keep them
	// working by forwarding to the Files section.
	if file := r.URL.Query().Get("file"); file != "" {
		http.Redirect(w, r, filesBack(pathID(r), file), http.StatusSeeOther)
		return
	}
	s.renderOverviewPage(w, r)
}

func (s *Server) renderOverviewPage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	page := overviewPage{projectPage: s.newProjectPage(r, p, role, "overview")}
	if files, err := s.store.ListWSFiles(r.Context(), p.ID); err == nil {
		page.FileCount = len(files)
	}
	if versions, err := s.store.ListWSVersions(r.Context(), p.ID); err == nil {
		page.VersionCount = len(versions)
	}
	if deployments, err := s.store.ListWSDeployments(r.Context(), p.ID); err == nil {
		page.DeploymentCount = len(deployments)
	}
	if triggers, err := s.store.ListWSTriggers(r.Context(), p.ID); err == nil {
		page.TriggerCount = len(triggers)
	}
	if execs, err := s.store.ListWSExecutions(r.Context(), p.ID, 5, 0); err == nil {
		page.RecentExecutions = execs
	}
	s.renderPage(w, http.StatusOK, "project_overview.gohtml", page)
}

/* ------------------------------------------------------------------ */
/* Files: list + editor                                                */
/* ------------------------------------------------------------------ */

type filesPage struct {
	projectPage
	Files    []*store.File
	Selected *store.File
	Result   *runResult // non-nil only in the response to a Run POST
}

func (s *Server) filesPageData(r *http.Request, p *store.WSProject, role store.Role, result *runResult, selectedName string) (*filesPage, error) {
	page := &filesPage{projectPage: s.newProjectPage(r, p, role, "files"), Result: result}
	var err error
	if page.Files, err = s.store.ListWSFiles(r.Context(), p.ID); err != nil {
		return nil, err
	}
	if selectedName != "" {
		for _, f := range page.Files {
			if f.Name == selectedName {
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

func (s *Server) renderFilesPage(w http.ResponseWriter, r *http.Request, status int, p *store.WSProject, role store.Role, result *runResult, selectedName string) {
	page, err := s.filesPageData(r, p, role, result, selectedName)
	if err != nil {
		s.renderPage(w, http.StatusInternalServerError, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Error"), Message: "could not load project"})
		return
	}
	s.renderPage(w, status, "project_files.gohtml", page)
}

func (s *Server) handleProjectFilesPage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	s.renderFilesPage(w, r, http.StatusOK, p, role, nil, r.URL.Query().Get("file"))
}

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
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if !pageMayEdit(role) {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d/files?err=%s", p.ID, urlQuery("you have view-only access")), http.StatusSeeOther)
		return
	}
	name := sanitizeFileName(r.FormValue("name"))
	back := fmt.Sprintf("/projects/%d/files", p.ID)
	if name == "" {
		http.Redirect(w, r, back+"?err="+urlQuery("a file needs a name"), http.StatusSeeOther)
		return
	}
	if _, err := s.store.UpsertWSFile(r.Context(), p.ID, name, newFileTemplate); err != nil {
		http.Redirect(w, r, back+"?err="+urlQuery("could not create file"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, filesBack(p.ID, name)+"&ok="+urlQuery("file created"), http.StatusSeeOther)
}

// handleSaveFilePage upserts the posted content. The editor's top bar makes
// the file name an editable field in the same form, so a changed name means
// rename: write the content under the new name, drop the old one.
func (s *Server) handleSaveFilePage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if !pageMayEdit(role) {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d/files?err=%s", p.ID, urlQuery("you have view-only access")), http.StatusSeeOther)
		return
	}
	oldName := r.PathValue("name")
	name := sanitizeFileName(r.FormValue("name"))
	if name == "" {
		name = oldName
	}
	content := r.FormValue("content")
	back := filesBack(p.ID, name)
	if len(content) > maxSourceBytes {
		http.Redirect(w, r, back+"&err="+urlQuery("file too large"), http.StatusSeeOther)
		return
	}
	if _, err := s.store.UpsertWSFile(r.Context(), p.ID, name, content); err != nil {
		http.Redirect(w, r, back+"&err="+urlQuery("could not save file"), http.StatusSeeOther)
		return
	}
	if name != oldName {
		if err := s.store.DeleteWSFile(r.Context(), p.ID, oldName); err != nil {
			http.Redirect(w, r, back+"&err="+urlQuery("renamed, but could not remove the old file"), http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, back+"&ok="+urlQuery("file saved"), http.StatusSeeOther)
}

func (s *Server) handleDeleteFilePage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if !pageMayEdit(role) {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d/files?err=%s", p.ID, urlQuery("you have view-only access")), http.StatusSeeOther)
		return
	}
	if err := s.store.DeleteWSFile(r.Context(), p.ID, r.PathValue("name")); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d/files?err=%s", p.ID, urlQuery("could not delete file")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d/files?ok=%s", p.ID, urlQuery("file deleted")), http.StatusSeeOther)
}

// handleRunFilePage runs the file and renders the Files page with the full
// result in place — the one POST that answers with a page rather than a
// redirect, because the fresh result (bytecode and app calls included)
// exists only in this response. Refreshing will re-POST, which re-runs; the
// execution is recorded either way.
//
// The editor form posts the buffer content and the (possibly edited) file
// name alongside the run request, so Run first persists exactly what is on
// screen — renames included — and the re-rendered textarea never loses the
// user's edits.
func (s *Server) handleRunFilePage(w http.ResponseWriter, r *http.Request) {
	p, role, user, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if !pageMayEdit(role) {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d/files?err=%s", p.ID, urlQuery("you have view-only access")), http.StatusSeeOther)
		return
	}
	oldName := r.PathValue("name")
	name := sanitizeFileName(r.FormValue("name"))
	if name == "" {
		name = oldName
	}
	if content, hasContent := r.PostForm["content"]; hasContent {
		if len(content[0]) > maxSourceBytes {
			http.Redirect(w, r, filesBack(p.ID, name)+"&err="+urlQuery("file too large"), http.StatusSeeOther)
			return
		}
		if _, err := s.store.UpsertWSFile(r.Context(), p.ID, name, content[0]); err != nil {
			http.Redirect(w, r, filesBack(p.ID, name)+"&err="+urlQuery("could not save file"), http.StatusSeeOther)
			return
		}
		if name != oldName {
			if err := s.store.DeleteWSFile(r.Context(), p.ID, oldName); err != nil {
				http.Redirect(w, r, filesBack(p.ID, name)+"&err="+urlQuery("renamed, but could not remove the old file"), http.StatusSeeOther)
				return
			}
		}
	}

	f, err := s.store.GetWSFile(r.Context(), p.ID, name)
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
	result, runErr := s.engine.RunWithActor(r.Context(), f.Content, actorFor(user, p))
	finished := time.Now()

	if runErr != nil && !errors.Is(runErr, engine.ErrBusy) && r.Context().Err() != nil {
		return // client went away; nothing to write back
	}
	exec, execErr := s.recordExecution(r.Context(), p, f, nil, "project-run", nil, nil, user, started, finished, result, runErr)
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
	s.renderFilesPage(w, r, http.StatusOK, p, role, &runResult{Result: result, ExecutionID: exec.ID}, name)
}

/* ------------------------------------------------------------------ */
/* Versions, deployments, triggers, KV                                 */
/* ------------------------------------------------------------------ */

type versionsPage struct {
	projectPage
	Versions []*store.Version
}

func (s *Server) handleProjectVersionsPage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	page := versionsPage{projectPage: s.newProjectPage(r, p, role, "versions")}
	var err error
	if page.Versions, err = s.store.ListWSVersions(r.Context(), p.ID); err != nil {
		s.renderPage(w, http.StatusInternalServerError, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Error"), Message: "could not load versions"})
		return
	}
	s.renderPage(w, http.StatusOK, "project_versions.gohtml", page)
}

func (s *Server) versionsBack(pID int64) string {
	return fmt.Sprintf("/projects/%d/versions", pID)
}

func (s *Server) handleCreateVersionPage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if !pageMayEdit(role) {
		http.Redirect(w, r, s.versionsBack(p.ID)+"?err="+urlQuery("you have view-only access"), http.StatusSeeOther)
		return
	}
	if _, err := s.store.CreateWSVersion(r.Context(), p.ID, r.FormValue("label")); err != nil {
		http.Redirect(w, r, s.versionsBack(p.ID)+"?err="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.versionsBack(p.ID)+"?ok="+urlQuery("version saved"), http.StatusSeeOther)
}

func (s *Server) handleRestoreVersionPage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if !pageMayEdit(role) {
		http.Redirect(w, r, s.versionsBack(p.ID)+"?err="+urlQuery("you have view-only access"), http.StatusSeeOther)
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		http.Redirect(w, r, s.versionsBack(p.ID)+"?err="+urlQuery("bad version number"), http.StatusSeeOther)
		return
	}
	v, err := s.store.GetWSVersionByNumber(r.Context(), p.ID, number)
	if err != nil {
		http.Redirect(w, r, s.versionsBack(p.ID)+"?err="+urlQuery("version not found"), http.StatusSeeOther)
		return
	}
	if err := s.store.RestoreWSVersion(r.Context(), p.ID, v.ID); err != nil {
		http.Redirect(w, r, s.versionsBack(p.ID)+"?err="+urlQuery("could not restore version"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.versionsBack(p.ID)+"?ok="+urlQuery("version restored"), http.StatusSeeOther)
}

type deploymentsPage struct {
	projectPage
	Deployments []*store.Deployment
	Files       []*store.File
}

func (s *Server) handleProjectDeploymentsPage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	page := deploymentsPage{projectPage: s.newProjectPage(r, p, role, "deployments")}
	var err error
	if page.Deployments, err = s.store.ListWSDeployments(r.Context(), p.ID); err != nil {
		s.renderPage(w, http.StatusInternalServerError, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Error"), Message: "could not load deployments"})
		return
	}
	if page.Files, err = s.store.ListWSFiles(r.Context(), p.ID); err != nil {
		s.renderPage(w, http.StatusInternalServerError, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Error"), Message: "could not load files"})
		return
	}
	s.renderPage(w, http.StatusOK, "project_deployments.gohtml", page)
}

func (s *Server) deploymentsBack(pID int64) string {
	return fmt.Sprintf("/projects/%d/deployments", pID)
}

func (s *Server) handleCreateDeploymentPage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if !pageMayEdit(role) {
		http.Redirect(w, r, s.deploymentsBack(p.ID)+"?err="+urlQuery("you have view-only access"), http.StatusSeeOther)
		return
	}
	f, err := s.store.GetWSFile(r.Context(), p.ID, r.FormValue("fileName"))
	if errors.Is(err, store.ErrNotFound) {
		http.Redirect(w, r, s.deploymentsBack(p.ID)+"?err="+urlQuery("file not found"), http.StatusSeeOther)
		return
	}
	if err != nil {
		http.Redirect(w, r, s.deploymentsBack(p.ID)+"?err="+urlQuery("could not look up file"), http.StatusSeeOther)
		return
	}
	if _, err := s.store.CreateWSDeployment(r.Context(), p.ID, f.ID, nil); err != nil {
		http.Redirect(w, r, s.deploymentsBack(p.ID)+"?err="+urlQuery(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.deploymentsBack(p.ID)+"?ok="+urlQuery("deployment created"), http.StatusSeeOther)
}

func (s *Server) handleToggleDeploymentPage(w http.ResponseWriter, r *http.Request) {
	s.mutateWSDeployment(w, r, func(depID int64, enabled bool) error {
		return s.store.SetWSDeploymentEnabled(r.Context(), depID, !enabled)
	})
}

func (s *Server) handleDeleteDeploymentPage(w http.ResponseWriter, r *http.Request) {
	s.mutateWSDeployment(w, r, func(depID int64, _ bool) error {
		return s.store.DeleteWSDeployment(r.Context(), depID)
	})
}

func (s *Server) mutateWSDeployment(w http.ResponseWriter, r *http.Request, fn func(depID int64, enabled bool) error) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if !pageMayEdit(role) {
		http.Redirect(w, r, s.deploymentsBack(p.ID)+"?err="+urlQuery("you have view-only access"), http.StatusSeeOther)
		return
	}
	depID, err := strconv.ParseInt(r.PathValue("depId"), 10, 64)
	if err != nil {
		http.Redirect(w, r, s.deploymentsBack(p.ID)+"?err="+urlQuery("bad deployment id"), http.StatusSeeOther)
		return
	}
	dep, err := s.store.GetWSDeployment(r.Context(), depID)
	if err != nil {
		http.Redirect(w, r, s.deploymentsBack(p.ID)+"?err="+urlQuery("deployment not found"), http.StatusSeeOther)
		return
	}
	if err := fn(depID, dep.Enabled); err != nil {
		http.Redirect(w, r, s.deploymentsBack(p.ID)+"?err="+urlQuery("could not update deployment"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.deploymentsBack(p.ID)+"?ok="+urlQuery("saved"), http.StatusSeeOther)
}

type triggersPage struct {
	projectPage
	Triggers []*store.Trigger
	Files    []*store.File
}

func (s *Server) handleProjectTriggersPage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	page := triggersPage{projectPage: s.newProjectPage(r, p, role, "triggers")}
	var err error
	if page.Triggers, err = s.store.ListWSTriggers(r.Context(), p.ID); err != nil {
		s.renderPage(w, http.StatusInternalServerError, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Error"), Message: "could not load triggers"})
		return
	}
	if page.Files, err = s.store.ListWSFiles(r.Context(), p.ID); err != nil {
		s.renderPage(w, http.StatusInternalServerError, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Error"), Message: "could not load files"})
		return
	}
	s.renderPage(w, http.StatusOK, "project_triggers.gohtml", page)
}

func (s *Server) triggersBack(pID int64) string {
	return fmt.Sprintf("/projects/%d/triggers", pID)
}

func (s *Server) handleCreateTriggerPage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if !pageMayEdit(role) {
		http.Redirect(w, r, s.triggersBack(p.ID)+"?err="+urlQuery("you have view-only access"), http.StatusSeeOther)
		return
	}
	backErr := func(msg string) {
		http.Redirect(w, r, s.triggersBack(p.ID)+"?err="+urlQuery(msg), http.StatusSeeOther)
	}
	f, err := s.store.GetWSFile(r.Context(), p.ID, r.FormValue("fileName"))
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

	next, err := scheduler.NextRun(time.Now(), nt.ScheduleType, *nt.IntervalSeconds, nt.DailyTimeUTC)
	if err != nil {
		backErr(err.Error())
		return
	}
	nt.NextRunAt = scheduler.FormatTime(next)

	if _, err := s.store.CreateWSTrigger(r.Context(), nt); err != nil {
		backErr(err.Error())
		return
	}
	http.Redirect(w, r, s.triggersBack(p.ID)+"?ok="+urlQuery("trigger created"), http.StatusSeeOther)
}

func (s *Server) handleToggleTriggerPage(w http.ResponseWriter, r *http.Request) {
	s.mutateWSTrigger(w, r, func(trigID int64, enabled bool) error {
		return s.store.SetWSTriggerEnabled(r.Context(), trigID, !enabled)
	})
}

func (s *Server) handleDeleteTriggerPage(w http.ResponseWriter, r *http.Request) {
	s.mutateWSTrigger(w, r, func(trigID int64, _ bool) error {
		return s.store.DeleteWSTrigger(r.Context(), trigID)
	})
}

func (s *Server) mutateWSTrigger(w http.ResponseWriter, r *http.Request, fn func(trigID int64, enabled bool) error) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if !pageMayEdit(role) {
		http.Redirect(w, r, s.triggersBack(p.ID)+"?err="+urlQuery("you have view-only access"), http.StatusSeeOther)
		return
	}
	trigID, err := strconv.ParseInt(r.PathValue("trigId"), 10, 64)
	if err != nil {
		http.Redirect(w, r, s.triggersBack(p.ID)+"?err="+urlQuery("bad trigger id"), http.StatusSeeOther)
		return
	}
	t, err := s.store.GetWSTrigger(r.Context(), trigID)
	if err != nil {
		http.Redirect(w, r, s.triggersBack(p.ID)+"?err="+urlQuery("trigger not found"), http.StatusSeeOther)
		return
	}
	if err := fn(trigID, t.Enabled); err != nil {
		http.Redirect(w, r, s.triggersBack(p.ID)+"?err="+urlQuery("could not update trigger"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.triggersBack(p.ID)+"?ok="+urlQuery("saved"), http.StatusSeeOther)
}

type kvPage struct {
	projectPage
	KV []*store.KVEntry
}

func (s *Server) handleProjectKVPage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	page := kvPage{projectPage: s.newProjectPage(r, p, role, "kv")}
	var err error
	if page.KV, err = s.store.ListWSKV(r.Context(), p.ID); err != nil {
		s.renderPage(w, http.StatusInternalServerError, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Error"), Message: "could not load kv"})
		return
	}
	s.renderPage(w, http.StatusOK, "project_kv.gohtml", page)
}

func (s *Server) handleDeleteKVPage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	if !pageMayEdit(role) {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d/kv?err=%s", p.ID, urlQuery("you have view-only access")), http.StatusSeeOther)
		return
	}
	if err := s.store.DeleteWSKV(r.Context(), p.ID, r.PathValue("key")); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/projects/%d/kv?err=%s", p.ID, urlQuery("could not delete kv entry")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%d/kv?ok=%s", p.ID, urlQuery("kv entry deleted")), http.StatusSeeOther)
}

/* ------------------------------------------------------------------ */
/* Executions                                                          */
/* ------------------------------------------------------------------ */

type executionsPage struct {
	projectPage
	Executions []*store.Execution
}

func (s *Server) handleProjectExecutionsPage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	page := executionsPage{projectPage: s.newProjectPage(r, p, role, "executions")}
	var err error
	if page.Executions, err = s.store.ListWSExecutions(r.Context(), p.ID, 50, 0); err != nil {
		s.renderPage(w, http.StatusInternalServerError, "error.gohtml",
			errorPage{basePage: s.basePage(r, "Error"), Message: "could not load executions"})
		return
	}
	s.renderPage(w, http.StatusOK, "project_executions.gohtml", page)
}

type executionPage struct {
	projectPage
	Exec     *store.Execution
	AppCalls []*store.AppCall
}

func (s *Server) handleExecutionPage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	execID, err := strconv.ParseInt(r.PathValue("execId"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	exec, err := s.store.GetWSExecution(r.Context(), execID)
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
	calls, _ := s.store.ListExecutionAppCalls(r.Context(), exec.ID)
	s.renderPage(w, http.StatusOK, "execution.gohtml",
		executionPage{
			projectPage: s.newProjectPage(r, p, role, "executions"),
			Exec:        exec, AppCalls: calls,
		})
}

/* ------------------------------------------------------------------ */
/* Logs: this project's app-call trail                                 */
/* ------------------------------------------------------------------ */

// logEntry is one app-call line from an app's operational log, tagged with
// the app it came from and a display-friendly timestamp.
type logEntry struct {
	App    string
	When   string
	ExecID int64
	applogs.Entry
}

type logsPage struct {
	projectPage
	Entries []logEntry
}

// handleProjectLogsPage shows the mail/http/logger operational records for
// this project — the same files the administration area's Logs pages tail,
// filtered to the project and newest first.
func (s *Server) handleProjectLogsPage(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.projectOrNotFoundPage(w, r)
	if !ok {
		return
	}
	page := logsPage{projectPage: s.newProjectPage(r, p, role, "logs")}
	dir := applogs.Dir()
	for _, app := range []string{"mail", "http", "logger"} {
		for _, e := range applogs.Tail(dir, app, 200) {
			if e.ProjectID != p.ID {
				continue
			}
			when := e.Time
			if t, err := time.Parse(time.RFC3339Nano, e.Time); err == nil {
				when = t.UTC().Format("2006-01-02 15:04:05")
			}
			page.Entries = append(page.Entries, logEntry{App: app, When: when, ExecID: e.ExecID, Entry: e})
		}
	}
	sort.SliceStable(page.Entries, func(i, j int) bool {
		return page.Entries[i].Time > page.Entries[j].Time
	})
	if len(page.Entries) > 100 {
		page.Entries = page.Entries[:100]
	}
	s.renderPage(w, http.StatusOK, "project_logs.gohtml", page)
}

/* ------------------------------------------------------------------ */

// pathID pulls the {id} route value as an int64; 0 when absent.
func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

// urlQuery escapes a flash or file-name value for a query string.
func urlQuery(s string) string {
	return url.QueryEscape(s)
}
