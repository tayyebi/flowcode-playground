package main

import (
	"context"
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
)

// Server holds the configuration and the state that outlives a request.
type Server struct {
	CompilerPath string // fcc
	RunnerPath   string // fcplay, the tracing driver
	WorkDir      string
	WebRoot      string
	Timeout      time.Duration
	QueueWait    time.Duration
	TrustProxy   bool

	samples []Sample
	slots   chan struct{} // bounded concurrency for compile+run
	limiter *rateLimiter
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	srv, err := newServerFromEnv()
	if err != nil {
		log.Fatalf("startup: %v", err)
	}

	addr := ":" + env("PORT", "8080")
	done := make(chan struct{})
	go srv.limiter.run(done)

	httpServer := &http.Server{
		Addr:    addr,
		Handler: srv.routes(),
		// A slow client must not be able to hold a connection open forever.
		// WriteTimeout has to clear the run timeout with room to spare or a
		// legitimately slow program would have its response cut off.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      srv.Timeout*2 + 15*time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Shut down on SIGTERM so `docker compose down` doesn't sever in-flight
	// runs, and so the deferred work-dir cleanups get to finish.
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("flowcode playground listening on %s (%d samples, timeout %s)",
			addr, len(srv.samples), srv.Timeout)
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

	s := &Server{
		CompilerPath: env("FLOWCODE_FCC", "/usr/local/bin/fcc"),
		RunnerPath:   env("FLOWCODE_RUNNER", "/usr/local/bin/fcplay"),
		WorkDir:      env("PLAYGROUND_WORKDIR", "/run/play"),
		WebRoot:      env("PLAYGROUND_WEB_ROOT", "/srv/web"),
		Timeout:      timeout,
		QueueWait:    5 * time.Second,
		TrustProxy:   env("TRUST_PROXY", "") == "1",
		slots:        make(chan struct{}, concurrency),
		limiter:      newRateLimiter(rpm, burst, 10*time.Minute),
	}

	// Fail loudly at startup rather than returning 500s later: a missing
	// binary or unreadable samples directory is a broken image, and it should
	// be obvious from the first log line, not from user reports.
	if err := checkExecutable(s.CompilerPath); err != nil {
		return nil, err
	}
	if err := checkExecutable(s.RunnerPath); err != nil {
		return nil, err
	}
	if err := checkWritableDir(s.WorkDir); err != nil {
		return nil, err
	}

	samples, err := loadSamples(env("FLOWCODE_SAMPLES_DIR", "/opt/flowcode/samples"))
	if err != nil {
		return nil, err
	}
	s.samples = samples

	return s, nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/api/samples", s.handleSamples)
	mux.HandleFunc("/api/run", s.limiter.middleware(s.TrustProxy, s.handleRun))

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
	for _, p := range []string{s.CompilerPath, s.RunnerPath} {
		if err := checkExecutable(p); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status": "unhealthy",
				"error":  err.Error(),
			})
			return
		}
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

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
