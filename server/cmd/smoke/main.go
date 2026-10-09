// Command smoke is an end-to-end check against a running playground.
//
// It compiles and runs every offered sample through the HTTP API and asserts
// each one comes back clean, then exercises the error and limit paths. Point
// it at a running instance to satisfy yourself that a `docker compose up`
// deploy is actually working:
//
//	go run ./cmd/smoke http://localhost:8033
//
// When the instance sets PLAYGROUND_ADMIN_TOKEN, export the same value in the
// environment and the smoke test authenticates with it. It exits non-zero on
// the first category of failure, with the offending response.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const hello = "workflow: Smoke\n\nstep greeting:\n    emit\n        value = \"hi\"\nend\n"

// Runs through the project engine and must show up in the project's KV log:
// fcplay dumps `store set` lines into the trace and the server parses them
// back out, storing the run's final value per key.
const kvProbe = "workflow: KVProbe\n\n" +
	"step greeting:\n    emit\n        value = \"hi\"\nend\n\n" +
	"step saved:\n    store set\n        key = \"smoke.greeting\"\n        value = greeting\nend\n"

// runResponse mirrors engine.Result plus the execution id, the fields the
// checks below assert on.
type runResponse struct {
	Compile struct {
		ExitCode    int `json:"exitCode"`
		Diagnostics []struct {
			Level string `json:"level"`
			Line  int    `json:"line"`
		} `json:"diagnostics"`
	} `json:"compile"`
	Bytecode *struct {
		InstructionCount int `json:"instructionCount"`
	} `json:"bytecode"`
	Run *struct {
		ExitCode int    `json:"exitCode"`
		Stderr   string `json:"stderr"`
	} `json:"run"`
}

type smoke struct {
	base     string
	client   *http.Client
	token    string
	failures []string
}

func main() {
	base := "http://localhost:8033"
	if len(os.Args) > 1 {
		base = os.Args[1]
	}
	s := &smoke{
		base:   strings.TrimRight(base, "/"),
		client: &http.Client{Timeout: 30 * time.Second},
		token:  os.Getenv("PLAYGROUND_ADMIN_TOKEN"),
	}

	s.checkHealth()
	s.checkSamples()
	s.checkDiagnostics()
	s.checkCompileErrors()
	s.checkLimits()
	s.checkProjects()
	s.checkRateLimit()

	fmt.Println()
	if len(s.failures) > 0 {
		fmt.Printf("%d check(s) failed\n", len(s.failures))
		os.Exit(1)
	}
	fmt.Println("all checks passed")
}

func (s *smoke) check(cond bool, format string, args ...interface{}) {
	if cond {
		fmt.Printf("  ok    %s\n", fmt.Sprintf(format, args...))
	} else {
		fmt.Printf("  FAIL  %s\n", fmt.Sprintf(format, args...))
		s.failures = append(s.failures, fmt.Sprintf(format, args...))
	}
}

// do performs one JSON request, decoding a success body into out (when out is
// non-nil) and returning the status. A refused request is a valid outcome to
// assert on, not an error: error bodies are decoded into out the same way.
//
// This client makes more requests in a few seconds than the default rate
// limit allows, so by default it backs off and retries on a 429 the way any
// well-behaved client would. respectLimit=false sees the raw 429, which is
// how the rate-limit check below asserts the limiter works at all.
func (s *smoke) do(method, path string, body, out any, respectLimit bool) int {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}

	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequest(method, s.base+path, reader)
		if err != nil {
			return 0
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if s.token != "" {
			req.AddCookie(&http.Cookie{Name: "playground_admin", Value: s.token})
		}

		resp, err := s.client.Do(req)
		if err != nil {
			return 0
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if out != nil && len(raw) > 0 {
			_ = json.Unmarshal(raw, out)
		}
		if resp.StatusCode == http.StatusTooManyRequests && respectLimit && attempt < 5 {
			delay := time.Second
			if v, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && v > 0 {
				delay = time.Duration(min(v, 10)) * time.Second
			}
			time.Sleep(delay)
			continue
		}
		return resp.StatusCode
	}
}

func (s *smoke) postRun(source string, out *runResponse, respectLimit bool) int {
	return s.do(http.MethodPost, "/api/run", map[string]string{"source": source}, out, respectLimit)
}

func (s *smoke) checkHealth() {
	fmt.Println("health")
	var health struct {
		Status string `json:"status"`
	}
	status := s.do(http.MethodGet, "/healthz", nil, &health, true)
	s.check(status == http.StatusOK && health.Status == "ok", "healthz reports ok (got %d, %q)", status, health.Status)
}

func (s *smoke) checkSamples() {
	fmt.Println("samples")
	var offered []struct {
		ID     string `json:"id"`
		Source string `json:"source"`
	}
	status := s.do(http.MethodGet, "/api/samples", nil, &offered, true)
	s.check(status == http.StatusOK && len(offered) > 0, "%d samples served (got %d)", len(offered), status)

	for _, sample := range offered {
		var result runResponse
		status := s.postRun(sample.Source, &result, true)

		ok := status == http.StatusOK &&
			result.Compile.ExitCode == 0 &&
			len(result.Compile.Diagnostics) == 0 &&
			result.Run != nil && result.Run.ExitCode == 0 &&
			// The trace is the whole reason fcplay exists; an empty pane here
			// means the stock silent runtime crept back in.
			strings.Contains(result.Run.Stderr, "vm completed successfully") &&
			result.Bytecode != nil && result.Bytecode.InstructionCount > 0
		s.check(ok, "sample %s compiles, runs, and traces", sample.ID)
		if !ok {
			raw, _ := json.Marshal(result)
			fmt.Printf("        %s\n", truncate(string(raw), 400))
		}
	}
}

func (s *smoke) checkDiagnostics() {
	fmt.Println("diagnostics")
	// FlowCode has no comment syntax: this is a warning, and the program still
	// compiles and runs. Both halves of that matter.
	var result runResponse
	status := s.postRun(hello+"\n# not a comment\n", &result, true)
	oneWarning := len(result.Compile.Diagnostics) == 1 && result.Compile.Diagnostics[0].Level == "warning"
	s.check(status == http.StatusOK && oneWarning,
		"an unrecognised line warns with a line number (%d diagnostics)", len(result.Compile.Diagnostics))
	s.check(result.Run != nil, "a warning does not prevent execution")
}

func (s *smoke) checkCompileErrors() {
	fmt.Println("compile errors")
	duplicate := "workflow: Dup\n\nstep a:\n    emit\n        value = \"x\"\nend\n\nstep a:\n    emit\n        value = \"y\"\nend\n"
	var result runResponse
	status := s.postRun(duplicate, &result, true)
	s.check(status == http.StatusOK, "a failed compile is still HTTP 200")
	s.check(result.Compile.ExitCode != 0, "a duplicate step name fails compilation")
	s.check(result.Run == nil, "a failed compile skips execution")
}

func (s *smoke) checkLimits() {
	fmt.Println("limits")
	var result runResponse
	status := s.postRun(strings.Repeat("x", 70_000), &result, true)
	s.check(status == http.StatusRequestEntityTooLarge, "oversized source is refused with 413 (got %d)", status)

	status = s.postRun("", &result, true)
	s.check(status == http.StatusBadRequest, "empty source is refused with 400 (got %d)", status)
}

func (s *smoke) checkProjects() {
	fmt.Println("projects")
	// Probe first and skip gracefully: a deployed image built before Phase A
	// (saved/versioned projects, deployments, triggers) won't have this route
	// at all, and this check should stay useful against that image too.
	if probeStatus := s.do(http.MethodGet, "/api/projects", nil, nil, true); probeStatus == http.StatusNotFound {
		fmt.Println("  skip  /api/projects not present on this image (pre-Phase-A)")
		return
	}

	var project struct {
		ID int64 `json:"id"`
	}
	status := s.do(http.MethodPost, "/api/projects", map[string]string{"name": "Smoke Test Project"}, &project, true)
	s.check(status == http.StatusCreated, "create project (%d)", status)

	var saved struct {
		Content string `json:"content"`
	}
	status = s.do(http.MethodPut, fmt.Sprintf("/api/projects/%d/files/main.fc", project.ID),
		map[string]string{"content": hello}, &saved, true)
	s.check(status == http.StatusOK && saved.Content == hello, "save file (%d)", status)

	var run runResponse
	status = s.do(http.MethodPost, fmt.Sprintf("/api/projects/%d/files/main.fc/run", project.ID), nil, &run, true)
	s.check(status == http.StatusOK && run.Compile.ExitCode == 0 && run.Run != nil,
		"run file via the project engine, same shape as /api/run (%d)", status)

	// A file that stores: the KV log is parsed out of the run trace's
	// store-set dump, so this is what proves that pipeline end to end.
	status = s.do(http.MethodPut, fmt.Sprintf("/api/projects/%d/files/kv.fc", project.ID),
		map[string]string{"content": kvProbe}, nil, true)
	s.check(status == http.StatusOK, "save kv probe file (%d)", status)

	status = s.do(http.MethodPost, fmt.Sprintf("/api/projects/%d/files/kv.fc/run", project.ID), nil, &run, true)
	s.check(status == http.StatusOK && run.Run != nil && run.Run.ExitCode == 0, "run the kv probe file (%d)", status)

	var kv []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	status = s.do(http.MethodGet, fmt.Sprintf("/api/projects/%d/kv", project.ID), nil, &kv, true)
	found := false
	for _, e := range kv {
		if e.Key == "smoke.greeting" && e.Value == "hi" {
			found = true
		}
	}
	s.check(status == http.StatusOK && found, "a store set lands in the kv log with its final value (%d)", status)

	var version struct {
		Number int `json:"number"`
	}
	status = s.do(http.MethodPost, fmt.Sprintf("/api/projects/%d/versions", project.ID),
		map[string]string{"label": "v1"}, &version, true)
	s.check(status == http.StatusCreated && version.Number == 1, "save version (%d)", status)

	var deployment struct {
		Slug string `json:"slug"`
	}
	status = s.do(http.MethodPost, fmt.Sprintf("/api/projects/%d/deployments", project.ID),
		map[string]string{"fileName": "main.fc"}, &deployment, true)
	s.check(status == http.StatusCreated, "create deployment (%d)", status)

	var deployBody struct {
		Note string `json:"note"`
	}
	deployStatus := s.do(http.MethodGet, "/deploy/"+deployment.Slug, nil, &deployBody, true)
	s.check(deployStatus == http.StatusOK && deployBody.Note != "",
		"deployment responds and states its Phase A limitation (%d)", deployStatus)

	status = s.do(http.MethodPost, fmt.Sprintf("/api/projects/%d/triggers", project.ID), map[string]any{
		"fileName": "main.fc", "scheduleType": "interval", "intervalSeconds": 1,
	}, nil, true)
	s.check(status == http.StatusCreated, "create trigger (%d)", status)

	// The scheduler ticks on its own cadence (~15s), independent of how
	// short the trigger's own interval is, so give it a bounded window
	// rather than sleeping for a fixed guess.
	fired := false
	for i := 0; i < 20 && !fired; i++ {
		var executions []struct {
			Source string `json:"source"`
		}
		s.do(http.MethodGet, fmt.Sprintf("/api/projects/%d/executions", project.ID), nil, &executions, true)
		for _, e := range executions {
			if e.Source == "trigger" {
				fired = true
			}
		}
		if !fired {
			time.Sleep(2 * time.Second)
		}
	}
	s.check(fired, "a short-interval trigger fires within the poll window")

	status = s.do(http.MethodDelete, fmt.Sprintf("/api/projects/%d", project.ID), nil, nil, true)
	s.check(status == http.StatusNoContent, "delete project cleans up (cascades) (%d)", status)
}

func (s *smoke) checkRateLimit() {
	fmt.Println("rate limiting")
	// Deliberately burst without backing off. Left until last, because it
	// exhausts the bucket and every check after it would have to wait.
	saw429 := false
	var result runResponse
	for i := 0; i < 40 && !saw429; i++ {
		if status := s.postRun(hello, &result, false); status == http.StatusTooManyRequests {
			saw429 = true
		}
	}
	s.check(saw429, "a sustained burst is eventually rate limited")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
