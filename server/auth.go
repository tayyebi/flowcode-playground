package main

// OIDC authentication and cookie sessions. The provider is any
// discovery-based issuer; its three env vars are the only app configuration
// that lives in the environment (everything else is admin-managed in
// system_settings, because the server cannot read the database before it
// knows how to open it — but it can sure read it right after).

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

const (
	sessionCookieName = "pg_session"
	sessionTTL        = 30 * 24 * time.Hour
	stateCookieName   = "pg_oidc_state"
	stateTTL          = 10 * time.Minute
)

type auth struct {
	provider *oidc.Provider
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
	secret   []byte // HMAC key for session cookies (system_settings.__session_secret)
	redirect string // optional override from OIDC_REDIRECT_URL
}

func newAuth(ctx context.Context, issuer, clientID, clientSecret, redirect string) (*auth, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery against %s: %w", issuer, err)
	}
	return &auth{
		provider: provider,
		oauth: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
		redirect: redirect,
	}, nil
}

func (a *auth) callbackURL(r *http.Request) string {
	if a.redirect != "" {
		return a.redirect
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/api/auth/callback"
}

/* ------------------------------------------------------------------ */
/* Login / callback / logout                                           */
/* ------------------------------------------------------------------ */

func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	state := randomToken(16)
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/",
		MaxAge:   int(stateTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.auth.oauth.AuthCodeURL(state, oauth2.SetAuthURLParam("redirect_uri", s.auth.callbackURL(r))), http.StatusSeeOther)
}

func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if errStr := q.Get("error"); errStr != "" {
		http.Redirect(w, r, "/login?err="+queryEscape(errStr), http.StatusSeeOther)
		return
	}
	state := q.Get("state")
	c, err := r.Cookie(stateCookieName)
	if err != nil || c.Value == "" || c.Value != state {
		http.Redirect(w, r, "/login?err="+queryEscape("login session expired, try again"), http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookieName, Value: "", Path: "/", MaxAge: -1})

	ctx := r.Context()
	tok, err := s.auth.oauth.Exchange(ctx, q.Get("code"), oauth2.SetAuthURLParam("redirect_uri", s.auth.callbackURL(r)))
	if err != nil {
		http.Redirect(w, r, "/login?err="+queryEscape("token exchange failed"), http.StatusSeeOther)
		return
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok {
		http.Redirect(w, r, "/login?err="+queryEscape("provider returned no id_token"), http.StatusSeeOther)
		return
	}
	idt, err := s.auth.verifier.Verify(ctx, rawID)
	if err != nil {
		http.Redirect(w, r, "/login?err="+queryEscape("id_token verification failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	var claims struct {
		Sub     string `json:"sub"`
		Email   string `json:"email"`
		Name    string `json:"name"`
		Picture string `json:"picture"`
	}
	if err := idt.Claims(&claims); err != nil || claims.Sub == "" {
		http.Redirect(w, r, "/login?err="+queryEscape("id_token carried no subject"), http.StatusSeeOther)
		return
	}

	user, err := s.loginUser(ctx, claims.Sub, claims.Email, claims.Name, claims.Picture)
	if err != nil {
		http.Redirect(w, r, "/login?err="+queryEscape("could not record login"), http.StatusSeeOther)
		return
	}

	s.setSession(w, user.ID)
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

// loginUser upserts the user and guarantees they own a workspace: GAS has
// "My Projects" from the first second, so a personal workspace is created on
// first login with the user as its owner.
func (s *Server) loginUser(ctx context.Context, subject, email, name, picture string) (*store.User, error) {
	user, err := s.store.UpsertUserBySubject(ctx, subject, email, name, picture)
	if err != nil {
		return nil, err
	}
	if _, err := s.ensureWorkspace(ctx, user); err != nil {
		return nil, err
	}
	return user, nil
}

// ensureWorkspace returns the user's first workspace, creating a personal
// one when they have none. Login uses it for first-timers; project creation
// uses it so SQL-seeded admins and the service token work out of the box.
func (s *Server) ensureWorkspace(ctx context.Context, user *store.User) (int64, error) {
	ws, err := s.store.ListWorkspacesForUser(ctx, user.ID)
	if err != nil {
		return 0, err
	}
	if len(ws) > 0 {
		return ws[0].ID, nil
	}
	wsName := firstNonEmptyStr(user.Name, emailLocalPart(user.Email), "workspace")
	created, cerr := s.store.CreateWorkspace(ctx, wsName)
	if cerr != nil {
		return 0, cerr
	}
	if aerr := s.store.AddWorkspaceMember(ctx, created.ID, user.ID, store.RoleOwner); aerr != nil {
		return 0, aerr
	}
	return created.ID, nil
}

func (s *Server) handleLogoutPost(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

/* ------------------------------------------------------------------ */
/* Session cookies: uid|expiry hex, HMAC-SHA256 signed                 */
/* ------------------------------------------------------------------ */

func (s *Server) setSession(w http.ResponseWriter, userID int64) {
	expiry := time.Now().Add(sessionTTL).Unix()
	payload := fmt.Sprintf("%d|%d", userID, expiry)
	mac := hmac.New(sha256.New, s.auth.secret)
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    payload + "|" + sig,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// currentUser resolves the session to a user, or nil. Also accepts the
// internal service token (Bearer) so scripts and the smoke test can use the
// API without an OIDC round-trip.
func (s *Server) currentUser(r *http.Request) *store.User {
	if u, ok := s.userFromSession(r); ok {
		return u
	}
	return s.userFromServiceToken(r)
}

func (s *Server) userFromSession(r *http.Request) (*store.User, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil, false
	}
	parts := strings.Split(c.Value, "|")
	if len(parts) != 3 {
		return nil, false
	}
	payload := parts[0] + "|" + parts[1]
	mac := hmac.New(sha256.New, s.auth.secret)
	mac.Write([]byte(payload))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return nil, false
	}
	var userID, expiry int64
	if _, err := fmt.Sscanf(payload, "%d|%d", &userID, &expiry); err != nil {
		return nil, false
	}
	if time.Now().Unix() > expiry || userID <= 0 {
		return nil, false
	}
	u, err := s.store.GetUser(r.Context(), userID)
	if err != nil {
		return nil, false
	}
	return u, true
}

func (s *Server) userFromServiceToken(r *http.Request) *store.User {
	h := r.Header.Get("Authorization")
	if len(h) <= 7 || h[:7] != "Bearer " || s.serviceToken == "" {
		return nil
	}
	if !hmac.Equal([]byte(h[7:]), []byte(s.serviceToken)) {
		return nil
	}
	// The service token acts as the first admin — it exists before any user
	// has been promoted. Look up any admin, else any user, else give up.
	admins, err := s.store.ListUsers(r.Context())
	if err != nil {
		return nil
	}
	for _, u := range admins {
		if u.IsAdmin {
			return u
		}
	}
	if len(admins) > 0 {
		return admins[0]
	}
	return nil
}

/* ------------------------------------------------------------------ */
/* Middleware                                                          */
/* ------------------------------------------------------------------ */

// requireUserPage gates HTML routes: bounce to /login instead of a 401.
func (s *Server) requireUserPage(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.currentUser(r) != nil {
			next(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

func (s *Server) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.currentUser(r) != nil {
			next(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "login required")
	}
}

// requireAdminPage / requireAdmin gate the system administration area on
// the manually-promoted is_admin flag.
func (s *Server) requireAdminPage(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u := s.currentUser(r); u != nil && u.IsAdmin {
			next(w, r)
			return
		}
		http.Redirect(w, r, "/projects?err="+queryEscape("system administrators only"), http.StatusSeeOther)
	}
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u := s.currentUser(r); u != nil && u.IsAdmin {
			next(w, r)
			return
		}
		writeError(w, http.StatusForbidden, "system administrators only")
	}
}

/* ------------------------------------------------------------------ */

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b) //nolint: crypto/rand never fails on supported platforms
	return hex.EncodeToString(b)
}

func queryEscape(s string) string {
	// small wrapper so auth.go doesn't import net/url for one call
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' {
			b.WriteByte('+')
		} else if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xF])
		}
	}
	return b.String()
}

func emailLocalPart(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return ""
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
