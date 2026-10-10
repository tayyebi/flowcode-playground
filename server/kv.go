package main

import (
	"net/http"

	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

// handleListKV exposes the project's best-effort, write-side kv log for the
// dashboard's debug/audit panel. See store.KVEntry's doc comment: this is
// NOT a working key-value API a running script can read from.
func (s *Server) handleListKV(w http.ResponseWriter, r *http.Request) {
	p, _, _, ok := s.wsProjectOr404(w, r)
	if !ok {
		return
	}
	entries, err := s.store.ListWSKV(r.Context(), p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list kv entries")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleDeleteKV(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.wsProjectOr404(w, r)
	if !ok {
		return
	}
	if !requireRole(w, role, store.RoleEditor) {
		return
	}
	key := r.PathValue("key")
	if err := s.store.DeleteWSKV(r.Context(), p.ID, key); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete kv entry")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
