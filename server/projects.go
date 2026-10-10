package main

import (
	"errors"
	"net/http"

	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

// resolveWSProject loads the {id} project and the current user's effective
// role. System administrators (is_admin) act as owners everywhere; everyone
// else needs workspace membership or a share. The returned status is 404 —
// not 403 — for foreign projects, so ids are not enumerable.
func (s *Server) resolveWSProject(r *http.Request) (*store.WSProject, store.Role, *store.User, int, string) {
	user := s.currentUser(r)
	if user == nil {
		return nil, "", nil, http.StatusUnauthorized, "login required"
	}
	id, err := pathInt64Value(r, "id")
	if err != nil || id <= 0 {
		return nil, "", user, http.StatusBadRequest, "bad project id"
	}
	p, gerr := s.store.GetWSProject(r.Context(), id)
	if errors.Is(gerr, store.ErrNotFound) {
		return nil, "", user, http.StatusNotFound, "project not found"
	}
	if gerr != nil {
		return nil, "", user, http.StatusInternalServerError, "could not look up project"
	}
	role := store.RoleViewer
	if user.IsAdmin {
		role = store.RoleOwner
	} else {
		role, err = s.store.EffectiveProjectRole(r.Context(), p.ID, user.ID)
		if err != nil {
			return nil, "", user, http.StatusInternalServerError, "could not resolve access"
		}
		if role == "" {
			return nil, "", user, http.StatusNotFound, "project not found"
		}
	}
	return p, role, user, 0, ""
}

// wsProjectOr404 is the JSON-flavoured resolver.
func (s *Server) wsProjectOr404(w http.ResponseWriter, r *http.Request) (*store.WSProject, store.Role, *store.User, bool) {
	p, role, user, status, msg := s.resolveWSProject(r)
	if status != 0 {
		writeError(w, status, msg)
		return nil, "", nil, false
	}
	return p, role, user, true
}

// requireRole writes a 403 and returns false when the user's role is below
// need (viewer is read-only; owner/editor may mutate).
func requireRole(w http.ResponseWriter, role, need store.Role) bool {
	rank := map[store.Role]int{store.RoleViewer: 1, store.RoleEditor: 2, store.RoleOwner: 3}
	if rank[role] >= rank[need] {
		return true
	}
	writeError(w, http.StatusForbidden, "you do not have edit access to this project")
	return false
}

// userWorkspaceID returns the workspace new artifacts are created in,
// auto-creating a personal one if the user has none (SQL-seeded admins and
// the service token never went through OIDC first login).
func (s *Server) userWorkspaceID(r *http.Request, user *store.User) (int64, bool) {
	id, err := s.ensureWorkspace(r.Context(), user)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

/* ------------------------------------------------------------------ */
/* JSON API: projects                                                  */
/* ------------------------------------------------------------------ */

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "login required")
		return
	}
	projects, err := s.store.ListProjectsForUser(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list projects")
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

type createProjectRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "login required")
		return
	}
	var req createProjectRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	wsID, ok := s.userWorkspaceID(r, user)
	if !ok {
		writeError(w, http.StatusInternalServerError, "you have no workspace")
		return
	}
	p, err := s.store.CreateWSProject(r.Context(), wsID, user.ID, req.Name, req.Description)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create project")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	p, _, _, ok := s.wsProjectOr404(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

type updateProjectRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.wsProjectOr404(w, r)
	if !ok {
		return
	}
	if !requireRole(w, role, store.RoleEditor) {
		return
	}
	var req updateProjectRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		return
	}
	if req.Name != nil && *req.Name == "" {
		writeError(w, http.StatusBadRequest, "name cannot be empty")
		return
	}
	updated, err := s.store.UpdateWSProject(r.Context(), p.ID, req.Name, req.Description)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update project")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.wsProjectOr404(w, r)
	if !ok {
		return
	}
	if !requireRole(w, role, store.RoleOwner) {
		return
	}
	if err := s.store.DeleteWSProject(r.Context(), p.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete project")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
