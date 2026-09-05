package main

import (
	"crypto/subtle"
	"net/http"
)

const adminCookieName = "pg_admin"

// requireAdmin gates management routes behind a single shared-secret token.
//
// This is deliberately not a real auth system: one shared secret, no
// hashing, no sessions table, no users. When PLAYGROUND_ADMIN_TOKEN is unset
// (the default), management routes stay exactly as open as the rest of the
// playground always has been. This never gates /api/run, /api/samples,
// /healthz, or a deployment's public /deploy/{slug} URL.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	if s.AdminToken == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if s.adminTokenMatches(adminTokenFromRequest(r)) {
			next(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "admin token required")
	}
}

func adminTokenFromRequest(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); len(auth) > 7 && auth[:7] == "Bearer " {
		return auth[7:]
	}
	if c, err := r.Cookie(adminCookieName); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) adminTokenMatches(token string) bool {
	if s.AdminToken == "" || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.AdminToken)) == 1
}

type adminLoginRequest struct {
	Token string `json:"token"`
}

// handleAdminLogin lets the dashboard trade a pasted token for a cookie, so
// the browser doesn't need to attach an Authorization header to every fetch.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.AdminToken == "" {
		writeError(w, http.StatusBadRequest, "no admin token is configured on this server")
		return
	}

	var req adminLoginRequest
	if err := decodeJSON(w, r, &req, 1024); err != nil {
		return
	}
	if !s.adminTokenMatches(req.Token) {
		writeError(w, http.StatusUnauthorized, "incorrect token")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    req.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((30 * 24 * 60 * 60)),
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminCookieName, Value: "", Path: "/", MaxAge: -1,
	})
	w.WriteHeader(http.StatusNoContent)
}
