package main

import (
	"errors"
	"net/http"

	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	versions, err := s.store.ListVersions(r.Context(), p.ID)
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
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	var req createVersionRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		return
	}
	v, err := s.store.CreateVersion(r.Context(), p.ID, req.Label)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

type versionDetail struct {
	*store.Version
	Files []*store.VersionFile `json:"files"`
}

func (s *Server) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	number, ok := pathInt(w, r, "number")
	if !ok {
		return
	}
	v, err := s.store.GetVersionByNumber(r.Context(), p.ID, number)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not look up version")
		return
	}
	files, err := s.store.ListVersionFiles(r.Context(), v.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list version files")
		return
	}
	writeJSON(w, http.StatusOK, versionDetail{Version: v, Files: files})
}

func (s *Server) handleRestoreVersion(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	number, ok := pathInt(w, r, "number")
	if !ok {
		return
	}
	v, err := s.store.GetVersionByNumber(r.Context(), p.ID, number)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not look up version")
		return
	}
	if err := s.store.RestoreVersion(r.Context(), p.ID, v.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not restore version")
		return
	}
	files, err := s.store.ListFiles(r.Context(), p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list files")
		return
	}
	writeJSON(w, http.StatusOK, files)
}
