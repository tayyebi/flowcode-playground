// mail-mock — a throw-away SMTP sink with an inbox web GUI and a single
// JSON file as its database. Go stdlib only. Replaces Mailpit for the local
// test rig.
//
// SMTP  (LISTEN_SMTP, default :1025): plain SMTP, no auth, no TLS advertised
//
//	— exactly what the playground's MailApp.sendEmail expects to find
//	when the admin configures host `mail-mock`, port 1025.
//
// HTTP  (LISTEN_HTTP, default :8025): the inbox. `/` lists messages newest
//
//	first; `/view?id=N` shows one message in full (headers + body).
//
// Database: one file (default data/mail-mock.json) holding the last 200
// messages. Restart-stable. Run with: go run .
package main

import (
	"bufio"
	"encoding/json"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	smtpAddr = envOr("LISTEN_SMTP", ":1025")
	httpAddr = envOr("LISTEN_HTTP", ":8025")
	dbPath   = envOr("DB", "data/mail-mock.json")

	mu       sync.Mutex
	messages []Message
	nextID   int64
)

// Message is one received email; Raw holds the full DATA payload verbatim.
type Message struct {
	ID       int64  `json:"id"`
	Received string `json:"received"`
	From     string `json:"from"`
	To       string `json:"to"`
	Subject  string `json:"subject"`
	Raw      string `json:"raw"`
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	load()

	ln, err := net.Listen("tcp", smtpAddr)
	if err != nil {
		log.Fatalf("smtp listen: %v", err)
	}
	go serveSMTP(ln)

	http.HandleFunc("/", guiInbox)
	http.HandleFunc("/view", guiView)
	http.HandleFunc("/clear", guiClear)

	log.Printf("mail-mock listening: smtp %s, http %s, db %s", smtpAddr, httpAddr, dbPath)
	log.Fatal(http.ListenAndServe(httpAddr, nil))
}

/* ------------------------------------------------------------------ */
/* Database: one JSON file                                             */
/* ------------------------------------------------------------------ */

func load() {
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		return
	}
	json.Unmarshal(raw, &messages)
	for _, m := range messages {
		if m.ID >= nextID {
			nextID = m.ID + 1
		}
	}
}

func save() {
	raw, err := json.MarshalIndent(messages, "", "  ")
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

func store(from, to string, raw string) {
	subject := ""
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(strings.ToUpper(line), "SUBJECT:") {
			subject = strings.TrimSpace(line[len("Subject:"):])
			break
		}
	}
	mu.Lock()
	nextID++
	messages = append([]Message{{
		ID: nextID, Received: time.Now().UTC().Format(time.RFC3339),
		From: from, To: to, Subject: subject, Raw: raw,
	}}, messages...)
	if len(messages) > 200 {
		messages = messages[:200]
	}
	save()
	id := nextID
	mu.Unlock()
	log.Printf("mail: #%d from=%s to=%s subject=%q", id, from, to, subject)
}

/* ------------------------------------------------------------------ */
/* SMTP: the minimal server the playground speaks                      */
/* ------------------------------------------------------------------ */

func serveSMTP(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleConn(conn)
	}
}

func handleConn(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)

	reply := func(s string) {
		w.WriteString(s + "\r\n")
		w.Flush()
	}
	dataLine := func() (string, bool) {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", false
		}
		return strings.TrimRight(line, "\r\n"), true
	}

	reply("220 mail-mock ESMTP")
	from, to := "", ""
	for {
		line, ok := dataLine()
		if !ok {
			return
		}
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			reply("250-mail-mock")
			reply("250 OK")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			from = angle(line[len("MAIL FROM:"):])
			reply("250 OK")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			to = angle(line[len("RCPT TO:"):])
			reply("250 OK")
		case cmd == "DATA":
			reply("354 end with <CRLF>.<CRLF>")
			var b strings.Builder
			for {
				dl, ok := dataLine()
				if !ok {
					return
				}
				if dl == "." {
					break
				}
				b.WriteString(dl)
				b.WriteString("\n")
			}
			store(from, to, b.String())
			reply("250 queued")
		case cmd == "RSET":
			from, to = "", ""
			reply("250 OK")
		case cmd == "NOOP", strings.HasPrefix(cmd, "VRFY"):
			reply("250 OK")
		case cmd == "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 not implemented")
		}
	}
}

func angle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '<'); i >= 0 {
		if j := strings.IndexByte(s[i:], '>'); j > 0 {
			return s[i+1 : i+j]
		}
	}
	return s
}

/* ------------------------------------------------------------------ */
/* GUI                                                                 */
/* ------------------------------------------------------------------ */

var inboxTmpl = template.Must(template.New("inbox").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta http-equiv="refresh" content="10">
<title>mail-mock</title><style>` + css + `</style></head>
<body>
<h1>mail-mock <span class="muted">· {{.Count}} message(s), newest first, refreshing every 10s</span></h1>
<p class="note">A throw-away SMTP sink. Configure the playground's Mail app
with host <code>mail-mock</code>, port <code>1025</code>. Database: <code>{{.DB}}</code>.</p>
{{if .Messages}}
<table>
  <tr><th>#</th><th>When (UTC)</th><th>From</th><th>To</th><th>Subject</th><th></th></tr>
  {{range .Messages}}
  <tr>
    <td class="num">{{.ID}}</td>
    <td class="muted">{{.Received}}</td>
    <td>{{.From}}</td><td>{{.To}}</td><td>{{.Subject}}</td>
    <td><a href="/view?id={{.ID}}">open</a></td>
  </tr>
  {{end}}
</table>
<form method="post" action="/clear"><button class="danger">clear inbox</button></form>
{{else}}<p class="note">No mail yet.</p>{{end}}
</body></html>`))

var viewTmpl = template.Must(template.New("view").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>mail-mock · #{{.M.ID}}</title><style>` + css + `</style></head>
<body>
<h1>#{{.M.ID}} {{.M.Subject}}</h1>
<p class="muted">{{.M.Received}} · {{.M.From}} → {{.M.To}} · <a href="/">&larr; inbox</a></p>
<pre class="raw">{{.M.Raw}}</pre>
</body></html>`))

func guiInbox(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	mu.Lock()
	msgs := append([]Message{}, messages...)
	mu.Unlock()
	inboxTmpl.Execute(w, map[string]any{"Messages": msgs, "Count": len(msgs), "DB": dbPath})
}

func guiView(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	mu.Lock()
	defer mu.Unlock()
	for _, m := range messages {
		if m.ID == id {
			viewTmpl.Execute(w, map[string]any{"M": m})
			return
		}
	}
	http.NotFound(w, r)
}

func guiClear(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	messages = nil
	save()
	mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

const css = `
body{font:14px/1.5 -apple-system,system-ui,sans-serif;margin:2rem auto;max-width:60rem;padding:0 1rem;color:#1a1a1a}
h1{font-size:1.4rem}
.muted{opacity:.6;font-weight:400}.note{opacity:.75}
table{border-collapse:collapse;width:100%;margin:.5rem 0}
td,th{border:1px solid #e2e2e2;padding:.35rem .5rem;text-align:left;font-size:.92rem}
td.num{font-variant-numeric:tabular-nums}
button{padding:.35rem .8rem;border:1px solid #bbb;border-radius:6px;background:#f6f6f6;cursor:pointer}
button.danger{color:#b00020;border-color:#e0b4b4}
pre.raw{background:#f7f7f7;border:1px solid #e2e2e2;border-radius:8px;padding:1rem;white-space:pre-wrap;word-break:break-word}
code{background:#f4f4f4;padding:.05rem .3rem;border-radius:4px;font-size:.85em}
`
