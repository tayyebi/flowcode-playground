// mail-mock — a throw-away mail sink with an inbox web GUI and a single
// JSON file as its database. Go stdlib only. Replaces Mailpit for the local
// test rig.
//
// SMTP  (LISTEN_SMTP, default :1025): plain SMTP, no auth, no TLS advertised
//
//	— exactly what the playground's MailApp.sendEmail expects to find
//	when the admin configures host `mail-mock`, port 1025.
//
// IMAP  (LISTEN_IMAP, default :1143): a minimal read-only IMAP4rev1 server
//
//	serving the same inbox — what MailApp.read expects when the admin
//	configures host `mail-mock`, port 1143 with TLS unchecked. Just enough
//	protocol for go-imap: greeting+capability, LOGIN, SELECT, FETCH
//	(ENVELOPE BODY[]), NOOP, LOGOUT.
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
	"fmt"
	"html/template"
	"io"
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
	imapAddr = envOr("LISTEN_IMAP", ":1143")
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

	lnIMAP, err := net.Listen("tcp", imapAddr)
	if err != nil {
		log.Fatalf("imap listen: %v", err)
	}
	go serveIMAP(lnIMAP)

	http.HandleFunc("/", guiInbox)
	http.HandleFunc("/view", guiView)
	http.HandleFunc("/clear", guiClear)

	log.Printf("mail-mock listening: smtp %s, imap %s, http %s, db %s", smtpAddr, imapAddr, httpAddr, dbPath)
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
/* IMAP: the minimal read-only server MailApp.read expects            */
/* ------------------------------------------------------------------ */

func serveIMAP(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleIMAP(conn)
	}
}

type imapSession struct {
	conn net.Conn
	r    *bufio.Reader
}

func handleIMAP(conn net.Conn) {
	defer conn.Close()
	s := &imapSession{conn: conn, r: bufio.NewReader(conn)}
	s.line("* OK [CAPABILITY IMAP4rev1] mail-mock IMAP ready")
	for {
		tag, cmd, args, err := s.readCommand()
		if err != nil {
			return
		}
		if tag == "" || cmd == "" {
			continue
		}
		switch cmd {
		case "CAPABILITY":
			s.line("* CAPABILITY IMAP4rev1")
			s.ok(tag, "CAPABILITY completed")
		case "LOGIN":
			// Any credentials open the one inbox the mock owns.
			s.ok(tag, "LOGIN completed")
		case "SELECT", "EXAMINE":
			s.selectInbox(tag, cmd)
		case "FETCH":
			s.fetch(tag, args)
		case "NOOP":
			s.ok(tag, "NOOP completed")
		case "LOGOUT":
			s.line("* BYE mail-mock closing")
			s.ok(tag, "LOGOUT completed")
			return
		default:
			s.line(tag + " BAD unsupported in mail-mock")
		}
	}
}

func (s *imapSession) line(l string) {
	s.conn.Write([]byte(l + "\r\n"))
}

func (s *imapSession) ok(tag, what string) {
	s.line(tag + " OK " + what)
}

func (s *imapSession) rawLine() (string, error) {
	l, err := s.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(l, "\r\n"), nil
}

// readCommand reads one logical command line. IMAP literals — a line ending
// in {n} — are resolved by sending the "+" continuation and consuming n raw
// bytes as the next token, so quoted and literal credentials both parse.
func (s *imapSession) readCommand() (tag, cmd string, args []string, err error) {
	line, err := s.rawLine()
	if err != nil {
		return "", "", nil, err
	}
	var toks []string
	for {
		var lit int
		toks, line, lit = tokenize(line, toks)
		if lit == 0 {
			break
		}
		s.line("+")
		buf := make([]byte, lit)
		if _, err = io.ReadFull(s.r, buf); err != nil {
			return "", "", nil, err
		}
		toks = append(toks, string(buf))
		if line, err = s.rawLine(); err != nil {
			return "", "", nil, err
		}
	}
	if len(toks) == 0 {
		return "", "", nil, nil
	}
	if len(toks) > 1 {
		cmd = strings.ToUpper(toks[1])
	}
	if len(toks) > 2 {
		args = toks[2:]
	}
	return toks[0], cmd, args, nil
}

// tokenize scans space-separated atoms and quoted (backslash-escaped)
// strings off s into toks. The returned tail is unconsumed input; count is
// non-zero when that tail is exactly an {n} literal request, which the
// caller must continue before tokenizing further.
func tokenize(s string, toks []string) (out []string, rest string, count int) {
	for {
		s = strings.TrimLeft(s, " ")
		if s == "" {
			return toks, "", 0
		}
		if n, ok := literalReq(s); ok {
			return toks, s, n
		}
		if s[0] == '"' {
			var b strings.Builder
			i := 1
			for i < len(s) {
				if s[i] == '\\' && i+1 < len(s) {
					b.WriteByte(s[i+1])
					i += 2
					continue
				}
				if s[i] == '"' {
					i++
					break
				}
				b.WriteByte(s[i])
				i++
			}
			toks = append(toks, b.String())
			s = s[i:]
			continue
		}
		j := strings.IndexByte(s, ' ')
		if j < 0 {
			toks = append(toks, s)
			return toks, "", 0
		}
		toks = append(toks, s[:j])
		s = s[j:]
	}
}

func literalReq(s string) (int, bool) {
	if len(s) < 3 || s[0] != '{' || s[len(s)-1] != '}' {
		return 0, false
	}
	n, err := strconv.Atoi(s[1 : len(s)-1])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func (s *imapSession) selectInbox(tag, cmd string) {
	mu.Lock()
	n := len(messages)
	mu.Unlock()

	mode := "[READ-WRITE]"
	if cmd == "EXAMINE" {
		mode = "[READ-ONLY]"
	}
	s.line(fmt.Sprintf("* %d EXISTS", n))
	s.line("* 0 RECENT")
	s.line("* OK [UIDVALIDITY 1] UIDs valid")
	s.line(fmt.Sprintf("* OK [UIDNEXT %d] predicted next UID", n+1))
	s.line("* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)")
	s.ok(tag, mode+" "+cmd+" completed")
}

func (s *imapSession) fetch(tag string, args []string) {
	if len(args) == 0 {
		s.line(tag + " BAD FETCH needs a sequence set")
		return
	}
	mu.Lock()
	msgs := append([]Message{}, messages...)
	mu.Unlock()
	n := len(msgs)

	// The mock stores newest-first; IMAP sequence numbers count oldest-first,
	// so seq n is messages[0] — and MailApp.read's "last N" ranges line up.
	for _, seq := range expandSeqSet(args[0], n) {
		m := msgs[n-seq]
		s.conn.Write([]byte(fmt.Sprintf("* %d FETCH (ENVELOPE %s BODY[] {%d}\r\n", seq, envelope(m), len(m.Raw))))
		s.conn.Write([]byte(m.Raw))
		s.line(")")
	}
	s.ok(tag, "FETCH completed")
}

// expandSeqSet expands an IMAP sequence set ("5", "1:5", "3:*") against a
// mailbox of n messages into ascending sequence numbers.
func expandSeqSet(set string, n int) []int {
	var out []int
	for _, part := range strings.Split(set, ",") {
		lo, hi := strings.TrimSpace(part), strings.TrimSpace(part)
		if i := strings.IndexByte(part, ':'); i >= 0 {
			lo, hi = part[:i], part[i+1:]
		}
		a, okA := seqNum(lo, n)
		b, okB := seqNum(hi, n)
		if !okA || !okB {
			continue
		}
		if a > b {
			a, b = b, a
		}
		for x := a; x <= b; x++ {
			out = append(out, x)
		}
	}
	return out
}

func seqNum(tok string, n int) (int, bool) {
	if tok == "*" {
		return n, n > 0
	}
	v, err := strconv.Atoi(tok)
	if err != nil || v < 1 {
		return 0, false
	}
	if v > n {
		v = n
	}
	return v, v > 0
}

// envelope renders the RFC 3501 ENVELOPE for one message. Date/subject/from
// come from the stored fields; sender/reply-to mirror from, everything else
// is NIL — enough for go-imap's parser and MailApp.read's summary.
func envelope(m Message) string {
	date := "NIL"
	if t, err := time.Parse(time.RFC3339, m.Received); err == nil {
		date = quoteIMAP(t.Format("02 Jan 2006 15:04:05 -0700"))
	}
	from := envelopeAddr(m.From)
	to := envelopeAddr(m.To)
	return "(" + date + " " + quoteIMAP(sanitizeIMAP(m.Subject)) + " " +
		from + " " + from + " " + from + " " + to +
		" NIL NIL NIL NIL)"
}

// One address, double-parenthesised the way ENVELOPE wants it: the outer
// parens are the address-list, the inner ones the address itself.
func envelopeAddr(a string) string {
	a = strings.TrimSpace(a)
	if a == "" {
		return "NIL"
	}
	mailbox, host := a, ""
	if i := strings.LastIndexByte(a, '@'); i >= 0 {
		mailbox, host = a[:i], a[i+1:]
	}
	return "((NIL NIL " + quoteIMAP(sanitizeIMAP(mailbox)) + " " + quoteIMAP(sanitizeIMAP(host)) + "))"
}

func sanitizeIMAP(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

func quoteIMAP(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
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
