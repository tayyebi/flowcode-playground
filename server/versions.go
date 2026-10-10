package main

import (
	"errors"
	"net/http"

	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	p, _, _, ok := s.wsProjectOr404(w, r)
	if !ok {
		return
	}
	versions, err := s.store.ListWSVersions(r.Context(), p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list versions")
		return
	}
	writeJSON(w, http.StatusOK, versions)
}

type createVersionRequest struct {
	Label string `json:"label"`
}

func (s *Server) handleCreateVersion(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.wsProjectOr404(w, r)
	if !ok {
		return
	}
	if !requireRole(w, role, store.RoleEditor) {
		return
	}
	var req createVersionRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		return
	}
	v, err := s.store.CreateWSVersion(r.Context(), p.ID, req.Label)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	p, _, _, ok := s.wsProjectOr404(w, r)
	if !ok {
		return
	}
	number, ok := pathInt(w, r, "number")
	if !ok {
		return
	}
	v, err := s.store.GetWSVersionByNumber(r.Context(), p.ID, number)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not look up version")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleRestoreVersion(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.wsProjectOr404(w, r)
	if !ok {
		return
	}
	if !requireRole(w, role, store.RoleEditor) {
		return
	}
	number, ok := pathInt(w, r, "number")
	if !ok {
		return
	}
	v, err := s.store.GetWSVersionByNumber(r.Context(), p.ID, number)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not look up version")
		return
	}
	if err := s.store.RestoreWSVersion(r.Context(), p.ID, v.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not restore version")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "restored"})
}
