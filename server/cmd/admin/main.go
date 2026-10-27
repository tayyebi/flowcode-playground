// The administration area binary. It is a separate service from the user
// facing playground: its own OIDC client, its own login (the administrators
// allowlist table), and no user-side powers at all. Both binaries share one
// PostgreSQL database — the admin area edits system_settings and app_quotas
// directly, then pokes the app's internal reload endpoint so changes apply
// without a restart.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/db"
	"github.com/tayyebi/flowcode-playground/server/internal/settings"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

type Server struct {
	TrustProxy bool

	store       *store.Store
	settings    *settings.Manager
	auth        *auth
	sqlDB       *sql.DB
	internalURL string // app's unpublished /internal listener; "" = skip reload pokes
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	srv, err := newServerFromEnv()
	if err != nil {
		log.Fatalf("startup: %v", err)
	}

	addr := ":" + env("PORT", "8034")
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("administration area listening on %s", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-shutdown
	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
	if err := srv.sqlDB.Close(); err != nil {
		log.Printf("closing database: %v", err)
	}
}

func newServerFromEnv() (*Server, error) {
	issuer := os.Getenv("OIDC_ISSUER")
	clientID := os.Getenv("OIDC_CLIENT_ID")
	clientSecret := os.Getenv("OIDC_CLIENT_SECRET")
	if issuer == "" || clientID == "" || clientSecret == "" {
		return nil, errors.New("OIDC_ISSUER, OIDC_CLIENT_ID and OIDC_CLIENT_SECRET are required (the administration area uses its own OIDC client)")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, errors.New("DATABASE_URL is required")
	}

	sqlDB, err := db.Open(dsn, 4)
	if err != nil {
		return nil, err
	}

	// Generous enough for the OIDC discovery retry loop on a cold boot.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	st := store.New(sqlDB)
	mgr := settings.NewManager(st)
	if err := mgr.Load(ctx); err != nil {
		return nil, err
	}
	adminSecret, err := mgr.EnsureInternal(ctx, settings.KeyAdminSessionSecret, 32)
	if err != nil {
		return nil, err
	}

	provider, err := newAuth(ctx, issuer, clientID, clientSecret, os.Getenv("OIDC_REDIRECT_URL"))
	if err != nil {
		return nil, err
	}
	provider.secret = []byte(adminSecret)

	s := &Server{
		TrustProxy:  env("TRUST_PROXY", "") == "1",
		store:       st,
		settings:    mgr,
		auth:        provider,
		sqlDB:       sqlDB,
		internalURL: strings.TrimRight(os.Getenv("APP_INTERNAL_URL"), "/"),
	}

	// Bootstrap the allowlist from the environment, but only while it is
	// empty: a restart must not resurrect an administrator removed in the UI.
	if emails := parseEmailList(os.Getenv("ADMIN_EMAILS")); len(emails) > 0 {
		n, err := st.SeedAdministrators(ctx, emails)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			log.Printf("seeded %d administrator(s) from ADMIN_EMAILS", n)
		}
	}
	if n, _ := st.CountAdministrators(ctx); n == 0 {
		log.Printf("WARNING: the administrators table is empty — nobody can sign in. Set ADMIN_EMAILS and recreate the volume, or insert a row: INSERT INTO administrators (email) VALUES ('you@example.com');")
	}
	return s, nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// Login: the admin OIDC client, gated on the administrators allowlist.
	mux.HandleFunc("GET /admin", s.requireAdminPage(s.handleAdminHomePage))
	mux.HandleFunc("GET /admin/login", s.handleLoginPage)
	mux.HandleFunc("GET /admin/login/go", s.handleLoginStart)
	mux.HandleFunc("GET /admin/api/auth/callback", s.handleAuthCallback)
	mux.HandleFunc("POST /admin/logout", s.handleLogoutPost)

	page := s.requireAdminPage
	mux.HandleFunc("GET /admin/mail", page(s.handleAdminMailPage))
	mux.HandleFunc("POST /admin/mail", page(s.handleAdminMailSave))
	mux.HandleFunc("GET /admin/mail/logs", page(s.handleAdminMailLogsPage))
	mux.HandleFunc("GET /admin/http", page(s.handleAdminHTTPPage))
	mux.HandleFunc("POST /admin/http", page(s.handleAdminHTTPSave))
	mux.HandleFunc("GET /admin/http/logs", page(s.handleAdminHTTPLogsPage))
	mux.HandleFunc("GET /admin/logger", page(s.handleAdminLoggerPage))
	mux.HandleFunc("POST /admin/logger", page(s.handleAdminLoggerSave))
	mux.HandleFunc("GET /admin/logger/logs", page(s.handleAdminLoggerLogsPage))
	mux.HandleFunc("GET /admin/quotas", page(s.handleAdminQuotasPage))
	mux.HandleFunc("POST /admin/quotas", page(s.handleAdminQuotaSave))
	mux.HandleFunc("POST /admin/quotas/delete", page(s.handleAdminQuotaDelete))
	mux.HandleFunc("GET /admin/users", page(s.handleAdminUsersPage))
	mux.HandleFunc("GET /admin/executions", page(s.handleAdminExecutionsPage))
	mux.HandleFunc("GET /admin/general", page(s.handleAdminGeneralPage))
	mux.HandleFunc("POST /admin/general", page(s.handleAdminGeneralSave))
	mux.HandleFunc("POST /admin/service-token/rotate", page(s.handleAdminRotateServiceToken))

	mux.HandleFunc("GET /admin/administrators", page(s.handleAdminAdministratorsPage))
	mux.HandleFunc("POST /admin/administrators/add", page(s.handleAdminAdministratorAdd))
	mux.HandleFunc("POST /admin/administrators/remove", page(s.handleAdminAdministratorRemove))

	return logRequests(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.sqlDB.PingContext(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy", "error": "database: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" && rec.status == http.StatusOK {
			return
		}
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

// parseEmailList splits a comma (or whitespace) separated email list,
// dropping empties.
func parseEmailList(s string) []string {
	out := []string{}
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' || r == '\t' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
