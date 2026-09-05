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

	"github.com/tayyebi/flowcode-playground/server/internal/db"
	"github.com/tayyebi/flowcode-playground/server/internal/engine"
	"github.com/tayyebi/flowcode-playground/server/internal/scheduler"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

// Server holds the configuration and the state that outlives a request.
type Server struct {
	WebRoot    string
	TrustProxy bool
	AdminToken string

	samples       []Sample
	engine        *engine.Engine
	limiter       *rateLimiter
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
	go srv.limiter.run(done)
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
		log.Printf("flowcode playground listening on %s (%d samples, timeout %s)",
			addr, len(srv.samples), srv.engine.Timeout)
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
	timeout, err := time.ParseDuration(env("PLAYGROUND_TIMEOUT", "5s"))
	if err != nil {
		return nil, err
	}
	concurrency, err := strconv.Atoi(env("PLAYGROUND_MAX_CONCURRENT", "4"))
	if err != nil || concurrency < 1 {
		return nil, errors.New("PLAYGROUND_MAX_CONCURRENT must be a positive integer")
	}
	rpm, err := strconv.Atoi(env("PLAYGROUND_RATE_PER_MINUTE", "30"))
	if err != nil || rpm < 1 {
		return nil, errors.New("PLAYGROUND_RATE_PER_MINUTE must be a positive integer")
	}
	burst, err := strconv.Atoi(env("PLAYGROUND_RATE_BURST", "10"))
	if err != nil || burst < 1 {
		return nil, errors.New("PLAYGROUND_RATE_BURST must be a positive integer")
	}

	deployRpm, err := strconv.Atoi(env("PLAYGROUND_DEPLOY_RATE_PER_MINUTE", "60"))
	if err != nil || deployRpm < 1 {
		return nil, errors.New("PLAYGROUND_DEPLOY_RATE_PER_MINUTE must be a positive integer")
	}
	deployBurst, err := strconv.Atoi(env("PLAYGROUND_DEPLOY_RATE_BURST", "20"))
	if err != nil || deployBurst < 1 {
		return nil, errors.New("PLAYGROUND_DEPLOY_RATE_BURST must be a positive integer")
	}

	compilerPath := env("FLOWCODE_FCC", "/usr/local/bin/fcc")
	runnerPath := env("FLOWCODE_RUNNER", "/usr/local/bin/fcplay")
	workDir := env("PLAYGROUND_WORKDIR", "/run/play")
	dbPath := env("PLAYGROUND_DB_PATH", "/data/playground.db")
	adminToken := os.Getenv("PLAYGROUND_ADMIN_TOKEN")

	// Fail loudly at startup rather than returning 500s later: a missing
	// binary or unreadable samples directory is a broken image, and it should
	// be obvious from the first log line, not from user reports.
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

	eng := engine.New(compilerPath, runnerPath, workDir, timeout, 5*time.Second,
		maxOutputBytes, concurrency)
	st := store.New(sqlDB)

	if adminToken == "" {
		log.Print("PLAYGROUND_ADMIN_TOKEN not set — project management routes are open to anyone who can reach this server")
	}

	s := &Server{
		WebRoot:       env("PLAYGROUND_WEB_ROOT", "/srv/web"),
		TrustProxy:    env("TRUST_PROXY", "") == "1",
		AdminToken:    adminToken,
		engine:        eng,
		limiter:       newRateLimiter(rpm, burst, 10*time.Minute),
		deployLimiter: newRateLimiter(deployRpm, deployBurst, 10*time.Minute),
		store:         st,
		scheduler:     scheduler.New(st, eng),
		sqlDB:         sqlDB,
	}

	samples, err := loadSamples(env("FLOWCODE_SAMPLES_DIR", "/opt/flowcode/samples"))
	if err != nil {
		return nil, err
	}
	s.samples = samples

	return s, nil
}

const maxOutputBytes = 64 * 1024

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/api/samples", s.handleSamples)
	mux.HandleFunc("/api/run", s.limiter.middleware(s.TrustProxy, s.handleRun))

	mux.HandleFunc("POST /api/admin/login", s.handleAdminLogin)
	mux.HandleFunc("POST /api/admin/logout", s.handleAdminLogout)

	admin := s.requireAdmin

	mux.HandleFunc("GET /api/projects", admin(s.handleListProjects))
	mux.HandleFunc("POST /api/projects", admin(s.handleCreateProject))
	mux.HandleFunc("GET /api/projects/{id}", admin(s.handleGetProject))
	mux.HandleFunc("PATCH /api/projects/{id}", admin(s.handleUpdateProject))
	mux.HandleFunc("DELETE /api/projects/{id}", admin(s.handleDeleteProject))

	mux.HandleFunc("GET /api/projects/{id}/files", admin(s.handleListFiles))
	mux.HandleFunc("PUT /api/projects/{id}/files/{name}", admin(s.handleSaveFile))
	mux.HandleFunc("DELETE /api/projects/{id}/files/{name}", admin(s.handleDeleteFile))
	mux.HandleFunc("POST /api/projects/{id}/files/{name}/run", admin(s.handleRunFile))

	mux.HandleFunc("GET /api/projects/{id}/versions", admin(s.handleListVersions))
	mux.HandleFunc("POST /api/projects/{id}/versions", admin(s.handleCreateVersion))
	mux.HandleFunc("GET /api/projects/{id}/versions/{number}", admin(s.handleGetVersion))
	mux.HandleFunc("POST /api/projects/{id}/versions/{number}/restore", admin(s.handleRestoreVersion))

	mux.HandleFunc("GET /api/projects/{id}/deployments", admin(s.handleListDeployments))
	mux.HandleFunc("POST /api/projects/{id}/deployments", admin(s.handleCreateDeployment))
	mux.HandleFunc("PATCH /api/projects/{id}/deployments/{depId}", admin(s.handleUpdateDeployment))
	mux.HandleFunc("DELETE /api/projects/{id}/deployments/{depId}", admin(s.handleDeleteDeployment))

	mux.HandleFunc("GET /api/projects/{id}/triggers", admin(s.handleListTriggers))
	mux.HandleFunc("POST /api/projects/{id}/triggers", admin(s.handleCreateTrigger))
	mux.HandleFunc("PATCH /api/projects/{id}/triggers/{trigId}", admin(s.handleUpdateTrigger))
	mux.HandleFunc("DELETE /api/projects/{id}/triggers/{trigId}", admin(s.handleDeleteTrigger))

	mux.HandleFunc("GET /api/projects/{id}/executions", admin(s.handleListExecutions))
	mux.HandleFunc("GET /api/projects/{id}/executions/{execId}", admin(s.handleGetExecution))

	mux.HandleFunc("GET /api/projects/{id}/kv", admin(s.handleListKV))
	mux.HandleFunc("DELETE /api/projects/{id}/kv/{key}", admin(s.handleDeleteKV))

	// Public: no admin gate, since a deployment is meant to be reachable by
	// whoever has its URL, same posture as /api/run.
	mux.HandleFunc("/deploy/{slug}", s.deployLimiter.middleware(s.TrustProxy, s.handleDeploy))

	// Static assets last, on the catch-all, so the API routes win.
	mux.Handle("/", s.staticHandler())

	return logRequests(mux)
}

// staticHandler serves the built frontend, falling back to index.html so a
// deep link or a reloaded permalink lands on the app rather than a 404.
func (s *Server) staticHandler() http.Handler {
	fs := http.FileServer(http.Dir(s.WebRoot))
	index := filepath.Join(s.WebRoot, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(s.WebRoot, filepath.Clean("/"+r.URL.Path))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			// Vite fingerprints asset filenames, so they can be cached hard;
			// index.html must not be, or a redeploy serves stale asset refs.
			if r.URL.Path != "/" && r.URL.Path != "/index.html" {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fs.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, index)
	})
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
		"status":  "ok",
		"samples": len(s.samples),
	})
}

// checkWritableDir proves the work directory can actually hold a run.
//
// os.MkdirAll succeeds on an existing directory no matter who owns it, which is
// not enough: mounting a tmpfs over the path replaces the image's chowned
// directory with a root-owned one, and the failure would otherwise surface as a
// 500 on the first submission rather than at startup.
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
