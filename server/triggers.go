package main

import (
	"errors"
	"net/http"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/scheduler"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

func (s *Server) handleListTriggers(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	triggers, err := s.store.ListTriggers(r.Context(), p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list triggers")
		return
	}
	writeJSON(w, http.StatusOK, triggers)
}

type createTriggerRequest struct {
	FileName        string `json:"fileName"`
	VersionID       *int64 `json:"versionId"`
	ScheduleType    string `json:"scheduleType"` // 'interval' | 'daily'
	IntervalSeconds *int   `json:"intervalSeconds"`
	DailyTimeUTC    string `json:"dailyTimeUtc"`
}

func (s *Server) handleCreateTrigger(w http.ResponseWriter, r *http.Request) {
	p, ok := s.getProjectOr404(w, r)
	if !ok {
		return
	}
	var req createTriggerRequest
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

	interval := 0
	if req.IntervalSeconds != nil {
		interval = *req.IntervalSeconds
	}
	next, err := scheduler.NextRun(time.Now(), req.ScheduleType, interval, req.DailyTimeUTC)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	t, err := s.store.CreateTrigger(r.Context(), store.NewTrigger{
		ProjectID: p.ID, FileID: f.ID, VersionID: req.VersionID,
		ScheduleType: req.ScheduleType, IntervalSeconds: req.IntervalSeconds, DailyTimeUTC: req.DailyTimeUTC,
		NextRunAt: scheduler.FormatTime(next),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create trigger")
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

type updateTriggerRequest struct {
	Enabled *bool `json:"enabled"`
}

func (s *Server) handleUpdateTrigger(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.getProjectOr404(w, r); !ok {
		return
	}
	trigID, ok := pathInt64(w, r, "trigId")
	if !ok {
		return
	}
	var req updateTriggerRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		return
	}
	if req.Enabled != nil {
		if err := s.store.SetTriggerEnabled(r.Context(), trigID, *req.Enabled); err != nil {
			writeError(w, http.StatusInternalServerError, "could not update trigger")
			return
		}
	}
	t, err := s.store.GetTrigger(r.Context(), trigID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "trigger not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not look up trigger")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleDeleteTrigger(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.getProjectOr404(w, r); !ok {
		return
	}
	trigID, ok := pathInt64(w, r, "trigId")
	if !ok {
		return
	}
	if err := s.store.DeleteTrigger(r.Context(), trigID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete trigger")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
