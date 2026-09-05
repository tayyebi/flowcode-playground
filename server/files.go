package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/engine"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	files, err := s.store.ListFiles(r.Context(), p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list files")
		return
	}
	writeJSON(w, http.StatusOK, files)
}

type saveFileRequest struct {
	Content string `json:"content"`
}

func (s *Server) handleSaveFile(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "file name is required")
		return
	}
	var req saveFileRequest
	if err := decodeJSON(w, r, &req, maxSourceBytes+1024); err != nil {
		return
	}
	if len(req.Content) > maxSourceBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "file content too large")
		return
	}
	f, err := s.store.UpsertFile(r.Context(), p.ID, name, req.Content)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save file")
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := s.store.DeleteFile(r.Context(), p.ID, name); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete file")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// runResult wraps engine.Result with the execution row id it was recorded
// under, so the frontend can link "Run" output to the project's history.
type runResult struct {
	*engine.Result
	ExecutionID int64 `json:"executionId"`
}

func (s *Server) handleRunFile(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	f, err := s.store.GetFile(r.Context(), p.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not look up file")
		return
	}

	started := time.Now()
	result, runErr := s.engine.Run(r.Context(), f.Content)
	finished := time.Now()

	if runErr != nil {
		if errors.Is(runErr, engine.ErrBusy) {
			writeError(w, http.StatusServiceUnavailable, "playground is busy, try again shortly")
			return
		}
		if r.Context().Err() != nil {
			return
		}
	}

	exec, execErr := s.recordExecution(r.Context(), p.ID, f, nil, "project-run", nil, nil, started, finished, result, runErr)
	if execErr != nil {
		writeError(w, http.StatusInternalServerError, "could not record execution")
		return
	}
	if runErr != nil {
		writeError(w, http.StatusInternalServerError, "could not execute the program")
		return
	}

	writeJSON(w, http.StatusOK, runResult{Result: result, ExecutionID: exec.ID})
}

// recordExecution stores an execution row and, best-effort, any `store set`
// calls it produced. Shared by project runs, deployments, and triggers so
// the bookkeeping is identical no matter which path invoked the engine.
func (s *Server) recordExecution(
	ctx context.Context,
	projectID int64, f *store.File, versionID *int64, source string,
	deploymentID, triggerID *int64,
	started, finished time.Time, result *engine.Result, runErr error,
) (*store.Execution, error) {
	var fileID *int64
	fileName := ""
	if f != nil {
		fileID = &f.ID
		fileName = f.Name
	}

	exec, err := s.store.RecordExecution(ctx, store.NewExecution{
		ProjectID: projectID, FileID: fileID, FileName: fileName, VersionID: versionID,
		Source: source, DeploymentID: deploymentID, TriggerID: triggerID,
		StartedAt: started.UTC().Format(time.RFC3339), FinishedAt: finished.UTC().Format(time.RFC3339),
		Result: result, Err: runErr,
	})
	if err != nil {
		return nil, err
	}

	if result != nil && result.Run != nil {
		for k, v := range store.ExtractStoreSets(result.Run.Stderr) {
			s.store.UpsertKV(ctx, projectID, k, v, &exec.ID)
		}
	}
	return exec, nil
}
