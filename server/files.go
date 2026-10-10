package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/apps"
	"github.com/tayyebi/flowcode-playground/server/internal/engine"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

// maxSourceBytes caps one .fc file's content.
const maxSourceBytes = 64 * 1024

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.wsProjectOr404(w, r)
	if !ok {
		return
	}
	files, err := s.store.ListWSFiles(r.Context(), p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list files")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "role": role})
}

type saveFileRequest struct {
	Content string `json:"content"`
}

func (s *Server) handleSaveFile(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.wsProjectOr404(w, r)
	if !ok {
		return
	}
	if !requireRole(w, role, store.RoleEditor) {
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
	f, err := s.store.UpsertWSFile(r.Context(), p.ID, name, req.Content)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save file")
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	p, role, _, ok := s.wsProjectOr404(w, r)
	if !ok {
		return
	}
	if !requireRole(w, role, store.RoleEditor) {
		return
	}
	name := r.PathValue("name")
	if err := s.store.DeleteWSFile(r.Context(), p.ID, name); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete file")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// runResult wraps engine.Result with the execution row id it was recorded
// under, so the caller can link "Run" output to the project's history.
type runResult struct {
	*engine.Result
	ExecutionID int64 `json:"executionId"`
}

func (s *Server) handleRunFile(w http.ResponseWriter, r *http.Request) {
	p, role, user, ok := s.wsProjectOr404(w, r)
	if !ok {
		return
	}
	if !requireRole(w, role, store.RoleEditor) {
		return
	}
	name := r.PathValue("name")
	f, err := s.store.GetWSFile(r.Context(), p.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not look up file")
		return
	}

	started := time.Now()
	result, runErr := s.engine.RunWithActor(r.Context(), f.Content, actorFor(user, p))
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

	exec, execErr := s.recordExecution(r.Context(), p, f, nil, "project-run", nil, nil, user, started, finished, result, runErr)
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

// actorFor maps a user + project to the apps-service attribution used for
// quotas, call records, and Loki labels.
func actorFor(user *store.User, p *store.WSProject) apps.Actor {
	if user == nil || p == nil {
		return apps.Actor{}
	}
	return apps.Actor{UserID: user.ID, Email: user.Email, ProjectID: p.ID}
}

// ownerActor attributes trigger and deployment runs to the project's owner.
func (s *Server) ownerActor(ctx context.Context, p *store.WSProject) apps.Actor {
	if p == nil || p.CreatedBy <= 0 {
		return apps.Actor{ProjectID: 0}
	}
	u, err := s.store.GetUser(ctx, p.CreatedBy)
	if err != nil {
		return apps.Actor{UserID: p.CreatedBy, ProjectID: p.ID}
	}
	return apps.Actor{UserID: u.ID, Email: u.Email, ProjectID: p.ID}
}

// recordExecution stores a ws execution row (attributed to the acting user),
// best-effort `store set` KV extraction, and the run's mail/http app calls.
func (s *Server) recordExecution(
	ctx context.Context,
	p *store.WSProject, f *store.File, versionID *int64, source string,
	deploymentID, triggerID *int64, actor *store.User,
	started, finished time.Time, result *engine.Result, runErr error,
) (*store.Execution, error) {
	var fileID *int64
	fileName := ""
	if f != nil {
		fileID = &f.ID
		fileName = f.Name
	}
	var actorID *int64
	if actor != nil {
		id := actor.ID
		actorID = &id
	}

	exec, err := s.store.RecordWSSExecution(ctx, store.NewExecution{
		ProjectID: p.ID, FileID: fileID, FileName: fileName, VersionID: versionID,
		Source: source, DeploymentID: deploymentID, TriggerID: triggerID,
		StartedAt: started.UTC().Format(time.RFC3339), FinishedAt: finished.UTC().Format(time.RFC3339),
		Summary: result.Summary(), Err: runErr,
	}, actorID)
	if err != nil {
		return nil, err
	}

	// KV extraction mirrors the legacy RecordRunWithKV behaviour.
	if result != nil && result.Run != nil {
		for k, v := range store.ExtractStoreSets(result.Run.Stderr) {
			s.store.UpsertWSKV(ctx, p.ID, k, v, &exec.ID)
		}
	}

	// Persist mail/http call records (logger lives in Loki only).
	if result != nil && len(result.AppCalls) > 0 {
		calls := []*store.AppCall{}
		uid := actorID
		for _, c := range result.AppCalls {
			if !appRecordable(c.Name) {
				continue
			}
			calls = append(calls, &store.AppCall{
				ExecutionID: exec.ID, UserID: uid, App: appFamily(c.Name), Name: c.Name,
				Status: c.Status, Request: c.Request, Response: c.Response, DurationMs: c.DurationMs,
			})
		}
		if len(calls) > 0 {
			if cerr := s.store.RecordAppCalls(ctx, calls); cerr != nil {
				return exec, nil // transcript persistence is best-effort
			}
		}
	}
	return exec, nil
}

// appFamily maps an app name to its settings/quotas family.
func appFamily(name string) string {
	switch name {
	case "MailApp.sendEmail", "MailApp.read":
		return "mail"
	case "UrlFetchApp.fetch":
		return "http"
	case "Logger.log":
		return "logger"
	}
	return "other"
}

// appRecordable: mail/http transcripts are stored locally; logger entries
// live only in Loki.
func appRecordable(name string) bool {
	switch name {
	case "MailApp.sendEmail", "MailApp.read", "UrlFetchApp.fetch":
		return true
	}
	return false
}
