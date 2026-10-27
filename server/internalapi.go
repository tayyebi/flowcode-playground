package main

// The internal listener: a tiny token-authed API for the administration
// binary, which shares the PostgreSQL database but not this process. When
// the admin area saves settings or rotates the service token it writes the
// database directly and then pokes POST /internal/reload so this process
// drops its cached snapshot immediately instead of waiting for the periodic
// refresh below. The listener is bound to INTERNAL_ADDR and is never
// published — only compose-network peers can reach it.

import (
	"context"
	"crypto/hmac"
	"log"
	"net/http"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/settings"
)

func (s *Server) internalRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/reload", s.handleInternalReload)
	return mux
}

func (s *Server) handleInternalReload(w http.ResponseWriter, r *http.Request) {
	// The token is read from the database, not the in-memory copy, so a
	// previously rotated token keeps working even before this process
	// refreshed its cache.
	tok, err := s.store.GetSetting(r.Context(), settings.KeyServiceToken)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read service token")
		return
	}
	h := r.Header.Get("Authorization")
	if tok == "" || len(h) <= 7 || h[:7] != "Bearer " || !hmac.Equal([]byte(h[7:]), []byte(tok)) {
		writeError(w, http.StatusForbidden, "invalid service token")
		return
	}
	if err := s.refreshSettings(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
}

// refreshSettings reloads the cached snapshot and re-reads the service
// token, so changes made by the admin binary (or plain SQL) apply without a
// restart. Sandbox run-limits are the exception: the engine is built with
// them at boot, which the admin UI notes on the General page.
func (s *Server) refreshSettings(ctx context.Context) error {
	if err := s.settings.Load(ctx); err != nil {
		return err
	}
	if tok, err := s.store.GetSetting(ctx, settings.KeyServiceToken); err == nil && tok != "" {
		s.serviceToken.Store(&tok)
	}
	return nil
}

// refreshSettingsLoop is the safety net behind the notify endpoint: a slow
// periodic reload keeps this process converging on the database even when a
// notification is lost (or someone edits SQL by hand).
func (s *Server) refreshSettingsLoop(done <-chan struct{}) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := s.refreshSettings(context.Background()); err != nil {
				log.Printf("settings refresh failed: %v", err)
			}
		}
	}
}
