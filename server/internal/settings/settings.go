// Package settings holds the admin-managed configuration that used to live
// in environment variables: per-app credentials and limits, and run limits.
// Everything is stored as one JSON blob per group in system_settings and
// cached in an atomic pointer; saving through the admin area reloads the
// cache for the next request.
package settings

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"sync/atomic"

	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

// Mail covers MailApp.sendEmail (SMTP) and MailApp.read (IMAP). Passwords are
// write-only from the admin UI's point of view; the service reads the real
// values from here.
type Mail struct {
	Enabled      bool   `json:"enabled"`
	SMTPHost     string `json:"smtpHost"`
	SMTPPort     int    `json:"smtpPort"`
	SMTPUsername string `json:"smtpUsername"`
	SMTPPassword string `json:"smtpPassword,omitempty"`
	SMTPFrom     string `json:"smtpFrom"`
	IMAPHost     string `json:"imapHost"`
	IMAPPort     int    `json:"imapPort"`
	IMAPUsername string `json:"imapUsername"`
	IMAPPassword string `json:"imapPassword,omitempty"`
}

// HTTP covers UrlFetchApp.fetch.
type HTTP struct {
	Enabled          bool     `json:"enabled"`
	AllowedDomains   []string `json:"allowedDomains"` // empty = allow all
	TimeoutSeconds   int      `json:"timeoutSeconds"`
	ResponseCapBytes int      `json:"responseCapBytes"`
}

// Logger covers Logger.log. Entries are pushed to Loki; nothing is stored
// locally. Enabled=false or an empty PushURL is a hard error for Logger.log
// (strict mode).
type Logger struct {
	Enabled        bool   `json:"enabled"`
	PushURL        string `json:"pushUrl"`
	Username       string `json:"username"`
	Password       string `json:"password,omitempty"`
	Tenant         string `json:"tenant"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

// Quotas holds the default per-user rate limits, overridable per user via
// the app_quotas table.
type Quotas struct {
	MailPerMinute   int `json:"mailPerMinute"`
	HTTPPerMinute   int `json:"httpPerMinute"`
	LoggerPerMinute int `json:"loggerPerMinute"`
}

// Run holds sandbox limits. Applies at boot; changes need a restart.
type Run struct {
	TimeoutSeconds      int `json:"timeoutSeconds"`
	MaxConcurrent       int `json:"maxConcurrent"`
	RatePerMinute       int `json:"ratePerMinute"`
	RateBurst           int `json:"rateBurst"`
	DeployRatePerMinute int `json:"deployRatePerMinute"`
	DeployRateBurst     int `json:"deployRateBurst"`
	OutputCapBytes      int `json:"outputCapBytes"`
}

// Snapshot is the full configuration, immutable once loaded.
type Snapshot struct {
	Mail   Mail   `json:"mail"`
	HTTP   HTTP   `json:"http"`
	Logger Logger `json:"logger"`
	Quotas Quotas `json:"quotas"`
	Run    Run    `json:"run"`
}

func defaults() Snapshot {
	return Snapshot{
		Mail: Mail{Enabled: false, SMTPPort: 587, IMAPPort: 993},
		HTTP: HTTP{Enabled: true, TimeoutSeconds: 10, ResponseCapBytes: 64 * 1024},
		Logger: Logger{
			Enabled:        false,
			PushURL:        "", // e.g. http://loki:3100 — strict: Logger.log errors until set
			TimeoutSeconds: 5,
		},
		Quotas: Quotas{MailPerMinute: 10, HTTPPerMinute: 30, LoggerPerMinute: 60},
		Run: Run{
			TimeoutSeconds:      5,
			MaxConcurrent:       4,
			RatePerMinute:       30,
			RateBurst:           10,
			DeployRatePerMinute: 60,
			DeployRateBurst:     20,
			OutputCapBytes:      64 * 1024,
		},
	}
}

// Group names in system_settings. Internal __ keys are separate and are
// never rendered by the admin UI.
const (
	KeyMail   = "mail"
	KeyHTTP   = "http"
	KeyLogger = "logger"
	KeyQuotas = "quotas"
	KeyRun    = "run"

	KeySessionSecret = "__session_secret"
	KeyServiceToken  = "__service_token"
)

// Manager caches the current Snapshot and reloads it on demand.
type Manager struct {
	store *store.Store
	cur   atomic.Pointer[Snapshot]
}

func NewManager(st *store.Store) *Manager {
	m := &Manager{store: st}
	m.cur.Store(&Snapshot{})
	return m
}

// Load reads every group from the database over the defaults and installs
// the result as the current snapshot. Called at boot and after admin saves.
func (m *Manager) Load(ctx context.Context) error {
	s := defaults()
	if err := m.loadGroup(ctx, KeyMail, &s.Mail); err != nil {
		return err
	}
	if err := m.loadGroup(ctx, KeyHTTP, &s.HTTP); err != nil {
		return err
	}
	if err := m.loadGroup(ctx, KeyLogger, &s.Logger); err != nil {
		return err
	}
	if err := m.loadGroup(ctx, KeyQuotas, &s.Quotas); err != nil {
		return err
	}
	if err := m.loadGroup(ctx, KeyRun, &s.Run); err != nil {
		return err
	}
	m.cur.Store(&s)
	return nil
}

func (m *Manager) loadGroup(ctx context.Context, key string, v any) error {
	return m.store.GetSettingJSON(ctx, key, v)
}

// Current returns the live snapshot.
func (m *Manager) Current() Snapshot {
	return *m.cur.Load()
}

// SaveGroup marshals v into the group's row and reloads the cache. Secrets
// need special handling by the caller (see MergeSecret).
func (m *Manager) SaveGroup(ctx context.Context, key string, v any, updatedBy int64) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := m.store.PutSetting(ctx, key, string(raw), updatedBy); err != nil {
		return err
	}
	return m.Load(ctx)
}

// EnsureInternal generates a value for an internal key when missing and
// returns it. Used for the session-secret and the service token.
func (m *Manager) EnsureInternal(ctx context.Context, key string, bytes int) (string, error) {
	if v, err := m.store.GetSetting(ctx, key); err != nil {
		return "", err
	} else if v != "" {
		return v, nil
	}
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	v := hex.EncodeToString(buf)
	if err := m.store.PutSetting(ctx, key, v, 0); err != nil {
		return "", err
	}
	log.Printf("generated %s", key)
	return v, nil
}

// RotateInternal replaces an internal key's value (admin action).
func (m *Manager) RotateInternal(ctx context.Context, key string, bytes int) (string, error) {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	v := hex.EncodeToString(buf)
	if err := m.store.PutSetting(ctx, key, v, 0); err != nil {
		return "", err
	}
	return v, nil
}

// QuotasKeyFor maps an app family to its default per-minute limit.
func QuotasKeyFor(app string, q Quotas) int {
	switch app {
	case "mail":
		return q.MailPerMinute
	case "http":
		return q.HTTPPerMinute
	case "logger":
		return q.LoggerPerMinute
	}
	return 0
}
