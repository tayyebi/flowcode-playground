package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/apps"
	"github.com/tayyebi/flowcode-playground/server/internal/db"
	"github.com/tayyebi/flowcode-playground/server/internal/engine"
	"github.com/tayyebi/flowcode-playground/server/internal/scheduler"
	"github.com/tayyebi/flowcode-playground/server/internal/settings"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

// Server holds the configuration and the state that outlives a request.
//
// Configuration splits in two by necessity: the OIDC credentials live in the
// environment because they are needed to boot the identity layer itself, and
// everything else (app credentials, limits, quotas) is admin-managed rows in
// system_settings, loaded through the settings manager.
type Server struct {
	TrustProxy bool

	auth         *auth
	serviceToken string
	settings     *settings.Manager
	appsSvc      *apps.Service

	engine        *engine.Engine
	deployLimiter *rateLimiter
	store         *store.Store
	scheduler     *scheduler.Scheduler
	sqlDB         *sql.DB
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	srv, err := newServerFromEnv()
	if err != nil {
		log.Fatalf("startup: %v", err)
	}

	addr := ":" + env("PORT", "8033")
	done := make(chan struct{})
	go srv.deployLimiter.run(done)
	go srv.scheduler.Run(done)

	httpServer := &http.Server{
		Addr:    addr,
		Handler: srv.routes(),
		// A slow client must not be able to hold a connection open forever.
		// WriteTimeout has to clear the run timeout with room to spare or a
		// legitimately slow program would have its response cut off.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      srv.engine.Timeout*2 + 15*time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Shut down on SIGTERM so `docker compose down` doesn't sever in-flight
	// runs, and so the deferred work-dir cleanups get to finish.
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("flowcode playground listening on %s (timeout %s)",
			addr, srv.engine.Timeout)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-shutdown
	log.Println("shutting down")
	close(done)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
	if err := srv.sqlDB.Close(); err != nil {
		log.Printf("closing database: %v", err)
	}
}

func newServerFromEnv() (*Server, error) {
	// The only app configuration in the environment: the OIDC credentials.
	issuer := os.Getenv("OIDC_ISSUER")
	clientID := os.Getenv("OIDC_CLIENT_ID")
	clientSecret := os.Getenv("OIDC_CLIENT_SECRET")
	if issuer == "" || clientID == "" || clientSecret == "" {
		return nil, errors.New("OIDC_ISSUER, OIDC_CLIENT_ID and OIDC_CLIENT_SECRET are required")
	}
	oidcRedirect := os.Getenv("OIDC_REDIRECT_URL")

	compilerPath := env("FLOWCODE_FCC", "/usr/local/bin/fcc")
	runnerPath := env("FLOWCODE_RUNNER", "/usr/local/bin/fcplay")
	workDir := env("PLAYGROUND_WORKDIR", "/run/play")
	dbPath := env("PLAYGROUND_DB_PATH", "/data/playground.db")

	// Fail loudly at startup rather than returning 500s later: a missing
	// binary or an unreadable working directory is a broken deploy, and it
	// should be obvious from the first log line, not from user reports.
	if err := checkExecutable(compilerPath); err != nil {
		return nil, err
	}
	if err := checkExecutable(runnerPath); err != nil {
		return nil, err
	}
	if err := checkWritableDir(workDir); err != nil {
		return nil, err
	}
	if err := checkWritableDir(filepath.Dir(dbPath)); err != nil {
		return nil, err
	}

	sqlDB, err := db.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st := store.New(sqlDB)
	mgr := settings.NewManager(st)
	if err := mgr.Load(ctx); err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	sessionSecret, err := mgr.EnsureInternal(ctx, settings.KeySessionSecret, 32)
	if err != nil {
		return nil, fmt.Errorf("ensure session secret: %w", err)
	}
	serviceToken, err := mgr.EnsureInternal(ctx, settings.KeyServiceToken, 24)
	if err != nil {
		return nil, fmt.Errorf("ensure service token: %w", err)
	}

	provider, err := newAuth(ctx, issuer, clientID, clientSecret, oidcRedirect)
	if err != nil {
		return nil, err
	}
	provider.secret = []byte(sessionSecret)

	appsSvc := apps.New(st, mgr)

	run := mgr.Current().Run
	eng := engine.New(compilerPath, runnerPath, workDir,
		time.Duration(run.TimeoutSeconds)*time.Second, 5*time.Second,
		run.OutputCapBytes, run.MaxConcurrent)
	eng.Apps = appsSvc

	s := &Server{
		TrustProxy:    env("TRUST_PROXY", "") == "1",
		auth:          provider,
		serviceToken:  serviceToken,
		settings:      mgr,
		appsSvc:       appsSvc,
		engine:        eng,
		deployLimiter: newRateLimiter(run.DeployRatePerMinute, run.DeployRateBurst, 10*time.Minute),
		store:         st,
		scheduler:     scheduler.New(st, eng),
		sqlDB:         sqlDB,
	}

	return s, nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("GET /static/style.css", handleStaticCSS)

	// Auth: /login shows the (single-button) sign-in page, /login/go starts
	// the OIDC redirect, the provider comes back to /api/auth/callback.
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("GET /login/go", s.handleLoginStart)
	mux.HandleFunc("GET /api/auth/callback", s.handleAuthCallback)
	mux.HandleFunc("POST /logout", s.handleLogoutPost)

	// Server-rendered pages. Every action is a plain form POST.
	page := s.requireUserPage
	mux.HandleFunc("GET /projects", page(s.handleProjectsPage))
	mux.HandleFunc("POST /projects", page(s.handleProjectCreatePage))
	mux.HandleFunc("GET /projects/{id}", page(s.handleProjectPage))
	mux.HandleFunc("POST /projects/{id}/update", page(s.handleProjectUpdatePage))
	mux.HandleFunc("POST /projects/{id}/delete", page(s.handleProjectDeletePage))
	mux.HandleFunc("POST /projects/{id}/files/new", page(s.handleNewFilePage))
	mux.HandleFunc("POST /projects/{id}/files/{name}/save", page(s.handleSaveFilePage))
	mux.HandleFunc("POST /projects/{id}/files/{name}/run", page(s.handleRunFilePage))
	mux.HandleFunc("POST /projects/{id}/files/{name}/delete", page(s.handleDeleteFilePage))
	mux.HandleFunc("POST /projects/{id}/versions", page(s.handleCreateVersionPage))
	mux.HandleFunc("POST /projects/{id}/versions/{number}/restore", page(s.handleRestoreVersionPage))
	mux.HandleFunc("POST /projects/{id}/deployments", page(s.handleCreateDeploymentPage))
	mux.HandleFunc("POST /projects/{id}/deployments/{depId}/toggle", page(s.handleToggleDeploymentPage))
	mux.HandleFunc("POST /projects/{id}/deployments/{depId}/delete", page(s.handleDeleteDeploymentPage))
	mux.HandleFunc("POST /projects/{id}/triggers", page(s.handleCreateTriggerPage))
	mux.HandleFunc("POST /projects/{id}/triggers/{trigId}/toggle", page(s.handleToggleTriggerPage))
	mux.HandleFunc("POST /projects/{id}/triggers/{trigId}/delete", page(s.handleDeleteTriggerPage))
	mux.HandleFunc("GET /projects/{id}/executions/{execId}", page(s.handleExecutionPage))
	mux.HandleFunc("POST /projects/{id}/kv/{key}/delete", page(s.handleDeleteKVPage))

	// System administration area: is_admin-gated sidebar pages.
	adminPage := s.requireAdminPage
	mux.HandleFunc("GET /admin", adminPage(s.handleAdminHomePage))
	mux.HandleFunc("GET /admin/mail", adminPage(s.handleAdminMailPage))
	mux.HandleFunc("POST /admin/mail", adminPage(s.handleAdminMailSave))
	mux.HandleFunc("GET /admin/mail/logs", adminPage(s.handleAdminMailLogsPage))
	mux.HandleFunc("GET /admin/http", adminPage(s.handleAdminHTTPPage))
	mux.HandleFunc("POST /admin/http", adminPage(s.handleAdminHTTPSave))
	mux.HandleFunc("GET /admin/http/logs", adminPage(s.handleAdminHTTPLogsPage))
	mux.HandleFunc("GET /admin/logger", adminPage(s.handleAdminLoggerPage))
	mux.HandleFunc("POST /admin/logger", adminPage(s.handleAdminLoggerSave))
	mux.HandleFunc("GET /admin/logger/logs", adminPage(s.handleAdminLoggerLogsPage))
	mux.HandleFunc("GET /admin/quotas", adminPage(s.handleAdminQuotasPage))
	mux.HandleFunc("POST /admin/quotas", adminPage(s.handleAdminQuotaSave))
	mux.HandleFunc("POST /admin/quotas/delete", adminPage(s.handleAdminQuotaDelete))
	mux.HandleFunc("GET /admin/users", adminPage(s.handleAdminUsersPage))
	mux.HandleFunc("GET /admin/executions", adminPage(s.handleAdminExecutionsPage))
	mux.HandleFunc("GET /admin/general", adminPage(s.handleAdminGeneralPage))
	mux.HandleFunc("POST /admin/general", adminPage(s.handleAdminGeneralSave))
	mux.HandleFunc("POST /admin/service-token/rotate", adminPage(s.handleAdminRotateServiceToken))

	// JSON API — same handlers, session or service-token auth.
	user := s.requireUser
	mux.HandleFunc("GET /api/projects", user(s.handleListProjects))
	mux.HandleFunc("POST /api/projects", user(s.handleCreateProject))
	mux.HandleFunc("GET /api/projects/{id}", user(s.handleGetProject))
	mux.HandleFunc("PATCH /api/projects/{id}", user(s.handleUpdateProject))
	mux.HandleFunc("DELETE /api/projects/{id}", user(s.handleDeleteProject))

	mux.HandleFunc("GET /api/projects/{id}/files", user(s.handleListFiles))
	mux.HandleFunc("PUT /api/projects/{id}/files/{name}", user(s.handleSaveFile))
	mux.HandleFunc("DELETE /api/projects/{id}/files/{name}", user(s.handleDeleteFile))
	mux.HandleFunc("POST /api/projects/{id}/files/{name}/run", user(s.handleRunFile))

	mux.HandleFunc("GET /api/projects/{id}/versions", user(s.handleListVersions))
	mux.HandleFunc("POST /api/projects/{id}/versions", user(s.handleCreateVersion))
	mux.HandleFunc("GET /api/projects/{id}/versions/{number}", user(s.handleGetVersion))
	mux.HandleFunc("POST /api/projects/{id}/versions/{number}/restore", user(s.handleRestoreVersion))

	mux.HandleFunc("GET /api/projects/{id}/deployments", user(s.handleListDeployments))
	mux.HandleFunc("POST /api/projects/{id}/deployments", user(s.handleCreateDeployment))
	mux.HandleFunc("PATCH /api/projects/{id}/deployments/{depId}", user(s.handleUpdateDeployment))
	mux.HandleFunc("DELETE /api/projects/{id}/deployments/{depId}", user(s.handleDeleteDeployment))

	mux.HandleFunc("GET /api/projects/{id}/triggers", user(s.handleListTriggers))
	mux.HandleFunc("POST /api/projects/{id}/triggers", user(s.handleCreateTrigger))
	mux.HandleFunc("PATCH /api/projects/{id}/triggers/{trigId}", user(s.handleUpdateTrigger))
	mux.HandleFunc("DELETE /api/projects/{id}/triggers/{trigId}", user(s.handleDeleteTrigger))

	mux.HandleFunc("GET /api/projects/{id}/executions", user(s.handleListExecutions))
	mux.HandleFunc("GET /api/projects/{id}/executions/{execId}", user(s.handleGetExecution))

	mux.HandleFunc("GET /api/projects/{id}/kv", user(s.handleListKV))
	mux.HandleFunc("DELETE /api/projects/{id}/kv/{key}", user(s.handleDeleteKV))

	// Public: no auth gate, since a deployment is meant to be reachable by
	// whoever has its URL. This public URL is also the webhook trigger —
	// the playground listens, the core never does.
	mux.HandleFunc("/deploy/{slug}", s.deployLimiter.middleware(s.TrustProxy, s.handleDeploy))

	return logRequests(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Re-stat the binaries rather than trusting the startup check: this is what
	// the container healthcheck polls, and its job is to notice breakage now.
	for _, p := range []string{s.engine.CompilerPath, s.engine.RunnerPath} {
		if err := checkExecutable(p); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status": "unhealthy",
				"error":  err.Error(),
			})
			return
		}
	}
	if err := s.sqlDB.PingContext(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unhealthy",
			"error":  "database: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
	})
}

// checkWritableDir proves the work directory can actually hold a run.
//
// os.MkdirAll succeeds on an existing directory no matter who owns it, which
// is not enough: mounting a tmpfs over the path replaces the image's chowned
// directory with a root-owned one, and the failure would otherwise surface as
// a 500 on the first submission rather than at startup.
func checkWritableDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	probe, err := os.MkdirTemp(path, "startup-")
	if err != nil {
		return fmt.Errorf("work dir %s is not writable: %w", path, err)
	}
	return os.RemoveAll(probe)
}

func checkExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return errors.New(path + " is not executable")
	}
	return nil
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// Deliberately no request body and no query string: submitted programs
		// are user content and have no business in the server's logs.
		if r.URL.Path == "/healthz" && rec.status == http.StatusOK {
			return // don't drown the log in healthcheck noise
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

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeJSON decodes a size-capped JSON request body, writing a 400/413
// response and returning a non-nil error if that fails — callers can just
// `if err := decodeJSON(...); err != nil { return }`.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return err
		}
		writeError(w, http.StatusBadRequest, "request body must be valid JSON")
		return err
	}
	return nil
}

// pathInt64Value parses an {id}-style path value without writing a response;
// resolveWSProject renders its own errors.
func pathInt64Value(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(r.PathValue(name), 10, 64)
}

// pathInt64 parses an {id}-style path value, writing a 400 and returning ok
// = false if it isn't a valid positive integer.
func pathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || v <= 0 {
		writeError(w, http.StatusBadRequest, name+" must be a positive integer")
		return 0, false
	}
	return v, true
}

// pathInt is pathInt64 for a smaller range, e.g. a version number.
func pathInt(w http.ResponseWriter, r *http.Request, name string) (int, bool) {
	v, ok := pathInt64(w, r, name)
	if !ok {
		return 0, false
	}
	return int(v), true
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
