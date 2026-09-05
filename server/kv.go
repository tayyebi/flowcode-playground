package main

import "net/http"

// handleListKV exposes the project's best-effort, write-side kv log for the
// dashboard's debug/audit panel. See store.KVEntry's doc comment: this is
// NOT a working key-value API a running script can read from.
func (s *Server) handleListKV(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	entries, err := s.store.ListKV(r.Context(), p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list kv entries")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleDeleteKV(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	if err := s.store.DeleteKV(r.Context(), p.ID, key); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete kv entry")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
