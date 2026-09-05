package main

import (
	"errors"
	"net/http"

	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

// getProjectOr404 looks up a project by its {id} path value, writing a 404
// and returning ok=false if it doesn't exist.
func (s *Server) getProjectOr404(w http.ResponseWriter, r *http.Request) (*store.Project, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return nil, false
	}
	p, err := s.store.GetProject(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "project not found")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not look up project")
		return nil, false
	}
	return p, true
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects(r.Context())
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
	var req createProjectRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	p, err := s.store.CreateProject(r.Context(), req.Name, req.Description)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create project")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
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
	p, ok := s.getProjectOr404(w, r)
	if !ok {
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
	updated, err := s.store.UpdateProject(r.Context(), p.ID, req.Name, req.Description)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update project")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteProject(r.Context(), p.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete project")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
