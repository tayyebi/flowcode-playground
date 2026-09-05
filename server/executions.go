package main

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

func (s *Server) handleListExecutions(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	var beforeID int64
	if v := r.URL.Query().Get("before"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			beforeID = n
		}
	}
	executions, err := s.store.ListExecutions(r.Context(), p.ID, limit, beforeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list executions")
		return
	}
	writeJSON(w, http.StatusOK, executions)
}

func (s *Server) handleGetExecution(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.getProjectOr404(w, r); !ok {
		return
	}
	execID, ok := pathInt64(w, r, "execId")
	if !ok {
		return
	}
	exec, err := s.store.GetExecution(r.Context(), execID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "execution not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not look up execution")
		return
	}
	writeJSON(w, http.StatusOK, exec)
}
