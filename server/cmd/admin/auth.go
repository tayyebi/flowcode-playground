package main

// OIDC login for the administration area, gated on the administrators
// allowlist table. This is the area's "own login": a separate OIDC client
// from the user-facing one, and a signed-in platform user is nobody here —
// only allowlisted identities get a session.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

const (
	adminSessionCookieName = "pg_admin_session"
	adminSessionTTL        = 12 * time.Hour
	adminStateCookieName   = "pg_admin_oidc_state"
	adminStateTTL          = 10 * time.Minute
)

type auth struct {
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
	secret   []byte // HMAC key for admin session cookies (system_settings.__admin_session_secret)
	redirect string // optional OIDC_REDIRECT_URL override
}

func newAuth(ctx context.Context, issuer, clientID, clientSecret, redirect string) (*auth, error) {
	// Retry discovery: on a cold compose boot the IdP (or the sidecar that
	// makes it reachable as "localhost") may not be listening yet, and a
	// startup error here crash-loops the service.
	provider, err := newProviderWithRetry(ctx, issuer)
	if err != nil {
		return nil, err
	}
	return &auth{
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

func newProviderWithRetry(ctx context.Context, issuer string) (*oidc.Provider, error) {
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		provider, err := oidc.NewProvider(ctx, issuer)
		if err == nil {
			return provider, nil
		}
		lastErr = fmt.Errorf("OIDC discovery against %s: %w", issuer, err)
		log.Printf("%v — retrying in 2s (%d/30)", lastErr, attempt+1)
		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(2 * time.Second):
		}
	}
	return nil, lastErr
}

// callbackURL builds the redirect URI. TLS terminates at nginx, so behind a
// trusted proxy the scheme comes from X-Forwarded-Proto.
func (s *Server) callbackURL(r *http.Request) string {
	if s.auth.redirect != "" {
		return s.auth.redirect
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if s.TrustProxy {
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = proto
		}
	}
	return scheme + "://" + r.Host + "/admin/api/auth/callback"
}

/* ------------------------------------------------------------------ */
/* Login / callback / logout                                           */
/* ------------------------------------------------------------------ */

func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	state := randomToken(16)
	http.SetCookie(w, &http.Cookie{
		Name:     adminStateCookieName,
		Value:    state,
		Path:     "/",
		MaxAge:   int(adminStateTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.auth.oauth.AuthCodeURL(state, oauth2.SetAuthURLParam("redirect_uri", s.callbackURL(r))), http.StatusSeeOther)
}

func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if errStr := q.Get("error"); errStr != "" {
		http.Redirect(w, r, "/admin/login?err="+queryEscape(errStr), http.StatusSeeOther)
		return
	}
	state := q.Get("state")
	c, err := r.Cookie(adminStateCookieName)
	if err != nil || c.Value == "" || c.Value != state {
		http.Redirect(w, r, "/admin/login?err="+queryEscape("login session expired, try again"), http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: adminStateCookieName, Value: "", Path: "/", MaxAge: -1})

	ctx := r.Context()
	tok, err := s.auth.oauth.Exchange(ctx, q.Get("code"), oauth2.SetAuthURLParam("redirect_uri", s.callbackURL(r)))
	if err != nil {
		http.Redirect(w, r, "/admin/login?err="+queryEscape("token exchange failed"), http.StatusSeeOther)
		return
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok {
		http.Redirect(w, r, "/admin/login?err="+queryEscape("provider returned no id_token"), http.StatusSeeOther)
		return
	}
	idt, err := s.auth.verifier.Verify(ctx, rawID)
	if err != nil {
		http.Redirect(w, r, "/admin/login?err="+queryEscape("id_token verification failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	var claims struct {
		Sub     string `json:"sub"`
		Email   string `json:"email"`
		Name    string `json:"name"`
		Picture string `json:"picture"`
	}
	if err := idt.Claims(&claims); err != nil || claims.Sub == "" {
		http.Redirect(w, r, "/admin/login?err="+queryEscape("id_token carried no subject"), http.StatusSeeOther)
		return
	}

	// The gate: the allowlist decides who is an administrator here. A
	// completely valid IdP identity that is not allowlisted is rejected.
	admin, err := s.store.FindAdministratorForLogin(ctx, claims.Sub, claims.Email)
	if err == store.ErrNotFound {
		http.Redirect(w, r, "/admin/login?err="+queryEscape("this account is not an administrator"), http.StatusSeeOther)
		return
	}
	if err != nil {
		http.Redirect(w, r, "/admin/login?err="+queryEscape("could not check the allowlist"), http.StatusSeeOther)
		return
	}
	if err := s.store.UpsertAdministratorLogin(ctx, admin.ID, claims.Sub, claims.Name, claims.Picture); err != nil {
		http.Redirect(w, r, "/admin/login?err="+queryEscape("could not record login"), http.StatusSeeOther)
		return
	}

	s.setAdminSession(w, admin.ID)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleLogoutPost(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: adminSessionCookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

/* ------------------------------------------------------------------ */
/* Session cookie: adminID|expiry hex, HMAC-SHA256 signed               */
/* ------------------------------------------------------------------ */

func (s *Server) setAdminSession(w http.ResponseWriter, adminID int64) {
	expiry := time.Now().Add(adminSessionTTL).Unix()
	payload := fmt.Sprintf("%d|%d", adminID, expiry)
	mac := hmac.New(sha256.New, s.auth.secret)
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    payload + "|" + sig,
		Path:     "/",
		MaxAge:   int(adminSessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// currentAdministrator resolves the session to an allowlist row, or nil.
// Looking the row up per request means removing an administrator also kills
// their live sessions.
func (s *Server) currentAdministrator(r *http.Request) *store.Administrator {
	c, err := r.Cookie(adminSessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	parts := strings.Split(c.Value, "|")
	if len(parts) != 3 {
		return nil
	}
	payload := parts[0] + "|" + parts[1]
	mac := hmac.New(sha256.New, s.auth.secret)
	mac.Write([]byte(payload))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return nil
	}
	var adminID, expiry int64
	if _, err := fmt.Sscanf(payload, "%d|%d", &adminID, &expiry); err != nil {
		return nil
	}
	if time.Now().Unix() > expiry || adminID <= 0 {
		return nil
	}
	admin, err := s.store.GetAdministrator(r.Context(), adminID)
	if err != nil {
		return nil
	}
	return admin
}

func (s *Server) requireAdminPage(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.currentAdministrator(r) != nil {
			next(w, r)
			return
		}
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
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
