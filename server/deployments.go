package main

import (
	"errors"
	"net/http"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/engine"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	p, _, _, ok := s.wsProjectOr404(w, r)
	if !ok {
		return
	}
	deployments, err := s.store.ListWSDeployments(r.Context(), p.ID)
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
	p, role, _, ok := s.wsProjectOr404(w, r)
	if !ok {
		return
	}
	if !requireRole(w, role, store.RoleEditor) {
		return
	}
	var req createDeploymentRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		return
	}
	f, err := s.store.GetWSFile(r.Context(), p.ID, req.FileName)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "fileName does not refer to a file in this project")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not look up file")
		return
	}
	dep, err := s.store.CreateWSDeployment(r.Context(), p.ID, f.ID, req.VersionID)
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
	if _, _, _, ok := s.wsProjectOr404(w, r); !ok {
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
		if err := s.store.SetWSDeploymentEnabled(r.Context(), depID, *req.Enabled); err != nil {
			writeError(w, http.StatusInternalServerError, "could not update deployment")
			return
		}
	}
	dep, err := s.store.GetWSDeployment(r.Context(), depID)
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
	if _, _, _, ok := s.wsProjectOr404(w, r); !ok {
		return
	}
	depID, ok := pathInt64(w, r, "depId")
	if !ok {
		return
	}
	if err := s.store.DeleteWSDeployment(r.Context(), depID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete deployment")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deployResponse is the public deployment call shape. The incoming request's
// method/query/body are ignored — this re-executes the file's workflow with
// no parameters and returns the raw result. (FlowCode's webhook-style
// triggering is host-side: this public URL IS the listener.)
type deployResponse struct {
	Deployment string `json:"deployment"`
	Note       string `json:"note"`
	*engine.Result
	ExecutionID int64 `json:"executionId,omitempty"`
}

const deployLimitationNote = "this deployment ignores the incoming request; it re-executes the workflow with no parameters"

// handleDeploy serves a project file's public, slug-addressed HTTP endpoint.
// It goes through the exact same engine.RunWithActor pipeline as every other
// execution path — nothing here duplicates the compile/run bridge, and the
// run is attributed to the project's owner for quotas and app-call records.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	dep, err := s.store.GetWSDeploymentBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !dep.Enabled) {
		writeError(w, http.StatusNotFound, "no such deployment, or it has been disabled")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not look up deployment")
		return
	}

	source, f, p, err := s.resolveDeploymentSource(r, dep)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not resolve deployment source")
		return
	}

	actor := s.ownerActor(r.Context(), p)

	started := time.Now()
	result, runErr := s.engine.RunWithActor(r.Context(), source, actor)
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

	var owner *store.User
	if p != nil && p.CreatedBy > 0 {
		if u, uerr := s.store.GetUser(r.Context(), p.CreatedBy); uerr == nil {
			owner = u
		}
	}
	exec, execErr := s.recordExecution(r.Context(), p, f, dep.VersionID, "deployment", &dep.ID, nil, owner, started, finished, result, nil)

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

func (s *Server) resolveDeploymentSource(r *http.Request, dep *store.Deployment) (string, *store.File, *store.WSProject, error) {
	f, err := s.store.GetWSFileByID(r.Context(), dep.FileID)
	if err != nil {
		return "", nil, nil, err
	}
	p, err := s.store.GetWSProject(r.Context(), f.ProjectID)
	if err != nil {
		return "", nil, nil, err
	}
	if dep.VersionID == nil {
		return f.Content, f, p, nil
	}
	content, err := s.store.GetWSVersionFileContent(r.Context(), *dep.VersionID, f.Name)
	if err != nil {
		return "", nil, nil, err
	}
	return content, f, p, nil
}
