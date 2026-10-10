// loki-mock — a throw-away Loki stand-in with a log-viewer web GUI and a
// single JSON file as its database. Go stdlib only. Replaces the Loki
// container for the local test rig.
//
// HTTP (LISTEN, default :3100):
//
//	POST /loki/api/v1/push        accepts the standard Loki push payload
//	                              ({"streams":[{"stream":{...},"values":
//	                              [[ts_ns, line], ...]}]}) — exactly what the
//	                              playground's Logger.log sends
//	GET  /loki/api/v1/query_range returns a Loki-shaped response (entries
//	                              grouped into streams, newest first) so the
//	                              admin Logger-logs viewer works unchanged
//	GET  /                        the GUI: recent entries + a filter box
//
// Database: one file (default data/loki-mock.json) holding the last 2000
// entries. Restart-stable. Run with: go run .
package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	addr   = envOr("LISTEN", ":3100")
	dbPath = envOr("DB", "data/loki-mock.json")

	mu      sync.Mutex
	entries []Entry
)

// Entry is one pushed log line with its labels.
type Entry struct {
	Nanos  int64             `json:"nanos"`
	Labels map[string]string `json:"labels"`
	Line   string            `json:"line"`
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	load()

	http.HandleFunc("/", gui)
	http.HandleFunc("/loki/api/v1/push", push)
	http.HandleFunc("/loki/api/v1/query_range", queryRange)

	log.Printf("loki-mock listening on %s, db %s", addr, dbPath)
	log.Fatal(http.ListenAndServe(addr, nil))
}

/* ------------------------------------------------------------------ */
/* Database: one JSON file                                             */
/* ------------------------------------------------------------------ */

func load() {
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		return
	}
	json.Unmarshal(raw, &entries)
}

func save() {
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	os.MkdirAll("data", 0o755)
	tmp := dbPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		log.Printf("write %s: %v", dbPath, err)
		return
	}
	os.Rename(tmp, dbPath)
}

func appendEntries(es []Entry) {
	mu.Lock()
	entries = append(entries, es...)
	// Newest last in the file; cap the tail.
	if len(entries) > 2000 {
		entries = entries[len(entries)-2000:]
	}
	save()
	n := len(es)
	mu.Unlock()
	log.Printf("loki: pushed %d entr%s", n, map[bool]string{true: "y", false: "ies"}[n == 1])
}

/* ------------------------------------------------------------------ */
/* Loki-compatible API                                                 */
/* ------------------------------------------------------------------ */

type pushPayload struct {
	Streams []struct {
		Stream map[string]string `json:"stream"`
		Values [][2]string       `json:"values"`
	} `json:"streams"`
}

func push(w http.ResponseWriter, r *http.Request) {
	var p pushPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "bad push payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UnixNano()
	es := []Entry{}
	for _, s := range p.Streams {
		for _, v := range s.Values {
			ts, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil || ts <= 0 {
				ts = now
			}
			es = append(es, Entry{Nanos: ts, Labels: s.Stream, Line: v[1]})
		}
	}
	appendEntries(es)
	w.WriteHeader(http.StatusNoContent)
}

type streamResult struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

func queryRange(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 1000 {
		limit = n
	}

	mu.Lock()
	snapshot := append([]Entry{}, entries...)
	mu.Unlock()

	// Newest first (direction=backward is what the admin viewer asks for).
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].Nanos > snapshot[j].Nanos })
	if len(snapshot) > limit {
		snapshot = snapshot[:limit]
	}

	streams := map[string]*streamResult{}
	order := []string{}
	for _, e := range snapshot {
		key := fmt.Sprint(e.Labels)
		sr, ok := streams[key]
		if !ok {
			sr = &streamResult{Stream: e.Labels}
			streams[key] = sr
			order = append(order, key)
		}
		sr.Values = append(sr.Values, [2]string{strconv.FormatInt(e.Nanos, 10), e.Line})
	}
	result := []streamResult{}
	for _, k := range order {
		result = append(result, *streams[k])
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status": "success",
		"data":   map[string]any{"result": result, "resultType": "streams"},
	})
}

/* ------------------------------------------------------------------ */
/* GUI                                                                 */
/* ------------------------------------------------------------------ */

var guiTmpl = template.Must(template.New("gui").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta http-equiv="refresh" content="10">
<title>loki-mock</title><style>` + css + `</style></head>
<body>
<h1>loki-mock <span class="muted">· {{.Count}} entr{{if ne .Count 1}}ies{{else}}y{{end}}, newest first, refreshing every 10s</span></h1>
<p class="note">A throw-away Loki stand-in. Point the playground's Logger app
at <code>http://loki-mock:3100</code>. Database: <code>{{.DB}}</code>.</p>
<form method="get" action="/" class="row">
  <input name="filter" value="{{.Filter}}" placeholder="filter: substring of the line or labels" size="48" />
  <button>filter</button>
  {{if .Filter}}<a href="/">clear</a>{{end}}
</form>
{{if .Rows}}
<table>
  <tr><th>When (UTC)</th><th>Labels</th><th>Line</th></tr>
  {{range .Rows}}
  <tr>
    <td class="muted">{{.When}}</td>
    <td class="muted"><code>{{.Labels}}</code></td>
    <td><code class="wrap">{{.Line}}</code></td>
  </tr>
  {{end}}
</table>
{{else}}<p class="note">No entries{{if .Filter}} match the filter{{end}} yet.</p>{{end}}
</body></html>`))

type guiRow struct {
	When, Labels, Line string
}

func gui(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	filter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("filter")))

	mu.Lock()
	snapshot := append([]Entry{}, entries...)
	mu.Unlock()
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].Nanos > snapshot[j].Nanos })
	if len(snapshot) > 300 {
		snapshot = snapshot[:300]
	}

	rows := []guiRow{}
	for _, e := range snapshot {
		labels := ""
		keys := make([]string, 0, len(e.Labels))
		for k := range e.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			labels += k + "=" + e.Labels[k] + " "
		}
		if filter != "" &&
			!strings.Contains(strings.ToLower(e.Line), filter) &&
			!strings.Contains(strings.ToLower(labels), filter) {
			continue
		}
		rows = append(rows, guiRow{
			When:   time.Unix(0, e.Nanos).UTC().Format(time.RFC3339),
			Labels: strings.TrimSpace(labels),
			Line:   e.Line,
		})
	}

	guiTmpl.Execute(w, map[string]any{
		"Rows": rows, "Count": len(rows), "Filter": filter, "DB": dbPath,
	})
}

const css = `
body{font:14px/1.5 -apple-system,system-ui,sans-serif;margin:2rem auto;max-width:64rem;padding:0 1rem;color:#1a1a1a}
h1{font-size:1.4rem}
.muted{opacity:.6;font-weight:400}.note{opacity:.75}
table{border-collapse:collapse;width:100%;margin:.5rem 0}
td,th{border:1px solid #e2e2e2;padding:.35rem .5rem;text-align:left;font-size:.92rem;vertical-align:top}
td code.wrap, code.wrap{white-space:pre-wrap;word-break:break-word}
input{padding:.35rem .5rem;border:1px solid #ccc;border-radius:6px;margin-right:.4rem}
button{padding:.35rem .8rem;border:1px solid #bbb;border-radius:6px;background:#f6f6f6;cursor:pointer}
form.row{margin:.45rem 0;display:flex;gap:.5rem;align-items:center}
code{background:#f4f4f4;padding:.05rem .3rem;border-radius:4px;font-size:.85em}
`
