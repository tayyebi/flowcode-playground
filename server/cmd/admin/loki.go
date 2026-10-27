package main

// The Logger-app log viewer: reads recent Logger.log entries straight from
// Loki's query_range API. The selector keeps labels low-cardinality: app +
// optional user.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/settings"
)

// lokiLine is one decoded log line from a query_range response.
type lokiLine struct {
	Timestamp string
	Labels    string
	Line      string
}

func (s *Server) queryLokiLines(r *http.Request) ([]lokiLine, string) {
	l := s.settings.Current().Logger
	if l.PushURL == "" {
		return nil, "logger is not configured — set a Loki push URL first"
	}
	return s.queryLoki(r.Context(), l, r.URL.Query().Get("user"))
}

func (s *Server) queryLoki(ctx context.Context, l settings.Logger, user string) ([]lokiLine, string) {
	selector := `{app="Logger.log"}`
	if user != "" {
		selector = fmt.Sprintf(`{app="Logger.log",user=%q}`, labelSafeUser(user))
	}
	base := strings.TrimSuffix(l.PushURL, "/")
	base = strings.TrimSuffix(base, "/loki/api/v1/push")
	q := url.Values{}
	q.Set("query", selector)
	q.Set("limit", "100")
	q.Set("start", strconv.FormatInt(time.Now().Add(-2*time.Hour).UnixNano(), 10))
	q.Set("end", strconv.FormatInt(time.Now().UnixNano(), 10))
	q.Set("direction", "backward")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/loki/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, err.Error()
	}
	if l.Tenant != "" {
		req.Header.Set("X-Scope-OrgID", l.Tenant)
	}
	if l.Username != "" {
		req.SetBasicAuth(l.Username, l.Password)
	}

	timeout := l.TimeoutSeconds
	if timeout <= 0 {
		timeout = 5
	}
	hc := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "loki query failed: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Sprintf("loki query returned HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Stream map[string]string `json:"stream"`
				Values [][2]string       `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Status != "success" {
		return nil, "loki returned an unparseable response"
	}

	out := []lokiLine{}
	for _, stream := range parsed.Data.Result {
		labels := fmt.Sprintf("user=%s project=%s", stream.Stream["user"], stream.Stream["project"])
		for _, v := range stream.Values {
			ts := v[0]
			if len(ts) >= 19 {
				if n, err := strconv.ParseInt(ts[:19], 10, 64); err == nil {
					ts = time.Unix(n, 0).UTC().Format(time.RFC3339)
				}
			}
			out = append(out, lokiLine{Timestamp: ts, Labels: labels, Line: v[1]})
		}
	}
	return out, ""
}

func labelSafeUser(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
