// oidc-mock — a throw-away OpenID Connect provider with a small web GUI and
// a single JSON file as its database. Go stdlib only.
//
// Endpoints the relying party (the playground) uses:
//
//	GET  /.well-known/openid-configuration   discovery document
//	GET  /auth?redirect_uri&state[&sub&email&name]
//	POST /auth/pick                          the GUI's identity picker
//	POST /token (authorization_code grant)   exchanges a one-time code for
//	                                         an RS256-signed id_token
//	GET  /jwks                               the public key
//
// GUI (http://localhost:9400/): the identity list (add/remove), the last
// logins, and the live issuer details. When /auth carries explicit identity
// params it auto-approves immediately (scripted flows); otherwise it renders
// the picker page so a browser login is one click.
//
// Database: one file (default data/oidc-mock.json) holding the identities,
// the signing key (PEM), and recent login events. Restart-stable.
//
// Run: ISSUER=http://localhost:9400 go run .
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	issuer = envOr("ISSUER", "http://localhost:9400")
	addr   = envOr("LISTEN", ":9400")
	dbPath = envOr("DB", "data/oidc-mock.json")

	mu    sync.Mutex
	state *State
	kid   string
	key   *rsa.PrivateKey
	codes = map[string]codeEntry{}
)

type Identity struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type LoginEvent struct {
	Time  string `json:"time"`
	Sub   string `json:"sub"`
	Email string `json:"email"`
	IP    string `json:"ip"`
}

// State is everything persisted — the whole database is this one struct.
type State struct {
	Identities []Identity   `json:"identities"`
	KeyPEM     string       `json:"keyPem"`
	Events     []LoginEvent `json:"events"`
}

type codeEntry struct {
	redirectURI      string
	sub, email, name string
	expires          time.Time
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	loadState()

	http.HandleFunc("/", guiHome)
	http.HandleFunc("/gui/add", guiAddIdentity)
	http.HandleFunc("/gui/remove", guiRemoveIdentity)
	http.HandleFunc("/.well-known/openid-configuration", discovery)
	http.HandleFunc("/jwks", jwks)
	http.HandleFunc("/auth", authStart)
	http.HandleFunc("/auth/pick", authPick)
	http.HandleFunc("/token", token)

	log.Printf("oidc-mock listening on %s (issuer %s, db %s, kid %s)", addr, issuer, dbPath, kid)
	log.Fatal(http.ListenAndServe(addr, nil))
}

/* ------------------------------------------------------------------ */
/* Database: one JSON file                                             */
/* ------------------------------------------------------------------ */

func loadState() {
	raw, err := os.ReadFile(dbPath)
	if err == nil {
		if jerr := json.Unmarshal(raw, &state); jerr == nil && state.KeyPEM != "" {
			deriveKey()
			return
		}
	}
	state = &State{
		Identities: []Identity{
			{Sub: "mock-user-1", Email: "mock@example.com", Name: "Mock User"},
		},
	}
	generateKey()
	save()
}

func save() {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	os.MkdirAll(dirOf(dbPath), 0o755)
	tmp := dbPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		log.Printf("write %s: %v", dbPath, err)
		return
	}
	os.Rename(tmp, dbPath)
}

func generateKey() {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("generate key: %v", err)
	}
	key = k
	state.KeyPEM = string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k),
	}))
	kid = randomHex(8)
}

func deriveKey() {
	block, _ := pem.Decode([]byte(state.KeyPEM))
	if block == nil {
		generateKey()
		return
	}
	k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		generateKey()
		return
	}
	key = k
	kid = randomHex(8)
}

func recordEvent(sub, email, ip string) {
	mu.Lock()
	defer mu.Unlock()
	state.Events = append([]LoginEvent{{
		Time: time.Now().UTC().Format(time.RFC3339), Sub: sub, Email: email, IP: ip,
	}}, state.Events...)
	if len(state.Events) > 100 {
		state.Events = state.Events[:100]
	}
	save()
}

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "."
}

/* ------------------------------------------------------------------ */
/* GUI                                                                 */
/* ------------------------------------------------------------------ */

var guiTmpl = template.Must(template.New("gui").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>oidc-mock</title><style>` + css + `</style></head>
<body>
<h1>oidc-mock <span class="muted">· {{.Issuer}}</span></h1>
<p class="note">A throw-away OpenID Connect provider. Sign-in requests open the
identity picker below; explicit <code>?sub=&amp;email=</code> params on
<code>/auth</code> auto-approve. Database: <code>{{.DB}}</code> (kid {{.Kid}}).</p>

<h2>Identities</h2>
<table>
  <tr><th>Sub</th><th>Email</th><th>Name</th><th></th></tr>
  {{range .Identities}}
  <tr>
    <td><code>{{.Sub}}</code></td><td>{{.Email}}</td><td>{{.Name}}</td>
    <td><form method="post" action="/gui/remove" class="inline">
      <input type="hidden" name="sub" value="{{.Sub}}" />
      <button class="danger">remove</button></form></td>
  </tr>
  {{end}}
</table>
<form method="post" action="/gui/add" class="row">
  <input name="sub" placeholder="sub (unique)" required />
  <input name="email" placeholder="email" required />
  <input name="name" placeholder="display name" />
  <button>add identity</button>
</form>

<h2>Recent logins</h2>
{{if .Events}}
<table>
  <tr><th>When (UTC)</th><th>Email</th><th>Sub</th><th>From</th></tr>
  {{range .Events}}<tr><td class="muted">{{.Time}}</td><td>{{.Email}}</td><td><code>{{.Sub}}</code></td><td class="muted">{{.IP}}</td></tr>{{end}}
</table>
{{else}}<p class="note">No logins yet.</p>{{end}}
</body></html>`))

func guiHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	guiTmpl.Execute(w, map[string]any{
		"Issuer": issuer, "DB": dbPath, "Kid": kid,
		"Identities": append([]Identity{}, state.Identities...),
		"Events":     append([]LoginEvent{}, state.Events...),
	})
}

func guiAddIdentity(w http.ResponseWriter, r *http.Request) {
	id := Identity{
		Sub:   strings.TrimSpace(r.FormValue("sub")),
		Email: strings.TrimSpace(r.FormValue("email")),
		Name:  strings.TrimSpace(r.FormValue("name")),
	}
	if id.Sub == "" || id.Email == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	mu.Lock()
	for _, existing := range state.Identities {
		if existing.Sub == id.Sub {
			mu.Unlock()
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	state.Identities = append(state.Identities, id)
	save()
	mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func guiRemoveIdentity(w http.ResponseWriter, r *http.Request) {
	sub := r.FormValue("sub")
	mu.Lock()
	kept := state.Identities[:0]
	for _, id := range state.Identities {
		if id.Sub != sub {
			kept = append(kept, id)
		}
	}
	state.Identities = kept
	save()
	mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

/* ------------------------------------------------------------------ */
/* OIDC: discovery / jwks                                              */
/* ------------------------------------------------------------------ */

func discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/auth",
		"token_endpoint":                        issuer + "/token",
		"jwks_uri":                              issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
		"grant_types_supported":                 []string{"authorization_code"},
	})
}

func jwks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
			"n": base64url(key.N.Bytes()),
			"e": base64url([]byte{1, 0, 1}), // 65537
		}},
	})
}

/* ------------------------------------------------------------------ */
/* OIDC: authorize (auto or picker) / token                            */
/* ------------------------------------------------------------------ */

var pickerTmpl = template.Must(template.New("picker").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Sign in — oidc-mock</title><style>` + css + `</style></head>
<body>
<h1>Sign in <span class="muted">· oidc-mock</span></h1>
<p class="note">Pick an identity. This is a mock provider: there are no passwords.</p>
{{range .Identities}}
<form method="post" action="/auth/pick" class="row">
  <input type="hidden" name="redirect_uri" value="{{$.RedirectURI}}" />
  <input type="hidden" name="state" value="{{$.State}}" />
  <input type="hidden" name="sub" value="{{.Sub}}" />
  <button>{{.Name}} &lt;{{.Email}}&gt;</button>
</form>
{{end}}
</body></html>`))

func authStart(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" {
		http.Error(w, "redirect_uri is required", http.StatusBadRequest)
		return
	}

	// Scripted flows: explicit identity parameters skip the picker.
	if q.Get("sub") != "" || q.Get("email") != "" {
		id := identityFor(q.Get("sub"), q.Get("email"), q.Get("name"))
		issueCodeRedirect(w, r, redirectURI, q.Get("state"), id)
		return
	}

	mu.Lock()
	ids := append([]Identity{}, state.Identities...)
	mu.Unlock()
	pickerTmpl.Execute(w, map[string]any{
		"Identities": ids, "RedirectURI": redirectURI, "State": q.Get("state"),
	})
}

func authPick(w http.ResponseWriter, r *http.Request) {
	redirectURI := r.FormValue("redirect_uri")
	if redirectURI == "" {
		http.Error(w, "redirect_uri is required", http.StatusBadRequest)
		return
	}
	id := identityFor(r.FormValue("sub"), "", "")
	issueCodeRedirect(w, r, redirectURI, r.FormValue("state"), id)
}

func identityFor(sub, email, name string) Identity {
	mu.Lock()
	defer mu.Unlock()
	if sub != "" {
		for _, id := range state.Identities {
			if id.Sub == sub {
				return id
			}
		}
	}
	if email != "" {
		for _, id := range state.Identities {
			if id.Email == email {
				return id
			}
		}
	}
	if sub != "" || email != "" {
		id := Identity{Sub: sub, Email: email, Name: name}
		if id.Sub == "" {
			id.Sub = email
		}
		if id.Name == "" {
			id.Name = id.Email
		}
		state.Identities = append(state.Identities, id)
		save()
		return id
	}
	return state.Identities[0]
}

func issueCodeRedirect(w http.ResponseWriter, r *http.Request, redirectURI, stateParam string, id Identity) {
	code := randomHex(16)
	mu.Lock()
	codes[code] = codeEntry{
		redirectURI: redirectURI, sub: id.Sub, email: id.Email, name: id.Name,
		expires: time.Now().Add(5 * time.Minute),
	}
	mu.Unlock()

	sep := "?"
	if strings.Contains(redirectURI, "?") {
		sep = "&"
	}
	loc := redirectURI + sep + "code=" + url.QueryEscape(code)
	if stateParam != "" {
		loc += "&state=" + url.QueryEscape(stateParam)
	}
	log.Printf("auth: %q <%s> approved -> %s", id.Name, id.Email, redirectURI)
	http.Redirect(w, r, loc, http.StatusFound)
}

func token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if got := r.PostForm.Get("grant_type"); got != "authorization_code" {
		http.Error(w, "only authorization_code is supported", http.StatusBadRequest)
		return
	}
	code := r.PostForm.Get("code")
	clientID := r.PostForm.Get("client_id")
	if clientID == "" {
		// x/oauth2 sends credentials via Basic auth on its first attempt;
		// the aud claim must match either way.
		if u, _, ok := r.BasicAuth(); ok {
			clientID = u
		}
	}

	mu.Lock()
	entry, ok := codes[code]
	delete(codes, code) // one-time use, like a real provider
	mu.Unlock()
	if !ok || time.Now().After(entry.expires) {
		http.Error(w, "invalid or expired code", http.StatusBadRequest)
		return
	}
	if ru := r.PostForm.Get("redirect_uri"); ru != entry.redirectURI {
		http.Error(w, "redirect_uri mismatch", http.StatusBadRequest)
		return
	}
	if clientID == "" {
		clientID = "mock-client"
	}

	idToken, err := signIDToken(clientID, entry)
	if err != nil {
		http.Error(w, "signing failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	recordEvent(entry.sub, entry.email, r.RemoteAddr)
	log.Printf("token: issued id_token for sub=%s email=%s", entry.sub, entry.email)
	writeJSON(w, map[string]any{
		"access_token": randomHex(16),
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

func signIDToken(aud string, e codeEntry) (string, error) {
	now := time.Now()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	claims := map[string]any{
		"iss":                issuer,
		"sub":                e.sub,
		"aud":                aud,
		"exp":                now.Add(time.Hour).Unix(),
		"iat":                now.Add(-time.Minute).Unix(),
		"email":              e.email,
		"email_verified":     true,
		"name":               e.name,
		"preferred_username": e.email,
	}
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := base64url(h) + "." + base64url(c)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64url(sig), nil
}

/* ------------------------------------------------------------------ */

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b) //nolint: never fails on supported platforms
	return fmt.Sprintf("%x", b)
}

func base64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

const css = `
body{font:14px/1.5 -apple-system,system-ui,sans-serif;margin:2rem auto;max-width:52rem;padding:0 1rem;color:#1a1a1a}
h1{font-size:1.4rem}h2{font-size:1.05rem;margin-top:1.6rem}
.muted{opacity:.6;font-weight:400}.note{opacity:.75}
table{border-collapse:collapse;width:100%;margin:.5rem 0}
td,th{border:1px solid #e2e2e2;padding:.35rem .5rem;text-align:left;font-size:.92rem}
input{padding:.35rem .5rem;border:1px solid #ccc;border-radius:6px;margin-right:.4rem}
button{padding:.35rem .8rem;border:1px solid #bbb;border-radius:6px;background:#f6f6f6;cursor:pointer}
button:hover{background:#ececec}button.danger{color:#b00020;border-color:#e0b4b4}
form.row{margin:.45rem 0}form.inline{display:inline}
code{background:#f4f4f4;padding:.05rem .3rem;border-radius:4px;font-size:.85em}
`
