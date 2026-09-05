package main

import (
	"errors"
	"net/http"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/engine"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	deployments, err := s.store.ListDeployments(r.Context(), p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list deployments")
		return
	}
	writeJSON(w, http.StatusOK, deployments)
}

type createDeploymentRequest struct {
	FileName  string `json:"fileName"`
	VersionID *int64 `json:"versionId"`
}

func (s *Server) handleCreateDeployment(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	var req createDeploymentRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		return
	}
	f, err := s.store.GetFile(r.Context(), p.ID, req.FileName)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "fileName does not refer to a file in this project")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not look up file")
		return
	}
	dep, err := s.store.CreateDeployment(r.Context(), p.ID, f.ID, req.VersionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create deployment")
		return
	}
	writeJSON(w, http.StatusCreated, dep)
}

type updateDeploymentRequest struct {
	Enabled *bool `json:"enabled"`
}

func (s *Server) handleUpdateDeployment(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.getProjectOr404(w, r); !ok {
		return
	}
	depID, ok := pathInt64(w, r, "depId")
	if !ok {
		return
	}
	var req updateDeploymentRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		return
	}
	if req.Enabled != nil {
		if err := s.store.SetDeploymentEnabled(r.Context(), depID, *req.Enabled); err != nil {
			writeError(w, http.StatusInternalServerError, "could not update deployment")
			return
		}
	}
	dep, err := s.store.GetDeployment(r.Context(), depID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "deployment not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not look up deployment")
		return
	}
	writeJSON(w, http.StatusOK, dep)
}

func (s *Server) handleDeleteDeployment(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.getProjectOr404(w, r); !ok {
		return
	}
	depID, ok := pathInt64(w, r, "depId")
	if !ok {
		return
	}
	if err := s.store.DeleteDeployment(r.Context(), depID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete deployment")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deployResponse is the Phase A shape for a public deployment call. See the
// plan doc: since FlowCode has no host-input channel, the incoming request's
// method/query/body are ignored — this re-executes the file's workflow with
// no parameters and returns the raw result, not something the workflow
// computed for this specific request. That limitation is stated explicitly
// here rather than glossed over, because a caller could otherwise reasonably
// assume this is a normal request/response web endpoint.
type deployResponse struct {
	Deployment string `json:"deployment"`
	Note       string `json:"note"`
	*engine.Result
	ExecutionID int64 `json:"executionId,omitempty"`
}

const deployLimitationNote = "this deployment ignores the incoming request; it re-executes the workflow with no parameters (Phase A limitation)"

// handleDeploy serves a project file's public, slug-addressed HTTP endpoint.
// It goes through the exact same engine.Run call as every other execution
// path in this codebase — nothing here duplicates the compile/run pipeline.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	dep, err := s.store.GetDeploymentBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !dep.Enabled) {
		writeError(w, http.StatusNotFound, "no such deployment, or it has been disabled")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not look up deployment")
		return
	}

	source, f, err := s.resolveDeploymentSource(r, dep)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not resolve deployment source")
		return
	}

	started := time.Now()
	result, runErr := s.engine.Run(r.Context(), source)
	finished := time.Now()

	w.Header().Set("X-FlowCode-Deploy-Note", deployLimitationNote)

	if runErr != nil {
		if errors.Is(runErr, engine.ErrBusy) {
			writeError(w, http.StatusServiceUnavailable, "deployment is busy, try again shortly")
			return
		}
		if r.Context().Err() != nil {
			return
		}
		writeError(w, http.StatusBadGateway, "the deployment failed to execute")
		return
	}

	exec, execErr := s.recordExecution(r.Context(), dep.ProjectID, f, dep.VersionID, "deployment", &dep.ID, nil, started, finished, result, nil)

	status := http.StatusOK
	if result.Compile.ExitCode != 0 || (result.Run != nil && result.Run.ExitCode != 0) {
		status = http.StatusBadGateway
	}

	resp := deployResponse{
		Deployment: dep.Slug,
		Note:       deployLimitationNote,
		Result:     result,
	}
	if execErr == nil {
		resp.ExecutionID = exec.ID
	}

	if r.URL.Query().Get("format") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		if result.Run != nil {
			w.Write([]byte(result.Run.Stderr))
		} else {
			w.Write([]byte(result.Compile.Stderr))
		}
		return
	}

	writeJSON(w, status, resp)
}

func (s *Server) resolveDeploymentSource(r *http.Request, dep *store.Deployment) (string, *store.File, error) {
	f, err := s.store.GetFileByID(r.Context(), dep.FileID)
	if err != nil {
		return "", nil, err
	}
	if dep.VersionID == nil {
		return f.Content, f, nil
	}
	content, err := s.store.GetVersionFileContent(r.Context(), *dep.VersionID, f.Name)
	if err != nil {
		return "", nil, err
	}
	return content, f, nil
}
