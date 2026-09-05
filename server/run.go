package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/tayyebi/flowcode-playground/server/internal/engine"
)

const maxSourceBytes = 64 * 1024

type runRequest struct {
	Source string `json:"source"`
}

// handleRun compiles and runs one submission.
//
// Failures at every stage are reported as a 200 with the details in the body:
// a program that doesn't compile is a normal, expected outcome of using a
// playground, not an HTTP error. Non-2xx is reserved for the request itself
// being unacceptable — too large, malformed, rate limited, or the server being
// unable to do the work at all.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Cap the body before decoding so an oversized upload is refused as it
	// arrives rather than after it has been buffered in full.
	r.Body = http.MaxBytesReader(w, r.Body, maxSourceBytes+1024)

	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("source must be at most %d bytes", maxSourceBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "request body must be JSON: {\"source\": \"...\"}")
		return
	}
	if len(req.Source) > maxSourceBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("source must be at most %d bytes", maxSourceBytes))
		return
	}
	if req.Source == "" {
		writeError(w, http.StatusBadRequest, "source is empty")
		return
	}

	resp, err := s.engine.Run(r.Context(), req.Source)
	if err != nil {
		if errors.Is(err, engine.ErrBusy) {
			writeError(w, http.StatusServiceUnavailable, "playground is busy, try again shortly")
			return
		}
		if r.Context().Err() != nil {
			return // client went away; nothing to write back
		}
		log.Printf("run failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not execute the program")
		return
	}

	writeJSON(w, http.StatusOK, resp)
}
