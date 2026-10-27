package store

import (
	"context"
	"fmt"
)

// administrators: the administration area's login allowlist. Identity comes
// from the admin OIDC client; a row may exist before the person ever signed
// in (added by email in the admin UI, or seeded from ADMIN_EMAILS at first
// boot) — subject stays NULL until their first login binds it. Platform
// users live in the separate users table; the two never mix.

// Administrator is one allowlisted operator of the administration area.
type Administrator struct {
	ID          int64  `json:"id"`
	Subject     string `json:"subject,omitempty"` // "" until first login
	Email       string `json:"email"`
	Name        string `json:"name"`
	Picture     string `json:"picture,omitempty"`
	CreatedAt   string `json:"createdAt"`
	LastLoginAt string `json:"lastLoginAt,omitempty"`
}

const administratorColumns = "id, COALESCE(subject, ''), email, name, picture, created_at, COALESCE(last_login_at, '')"

func scanAdministrator(row rowScanner) (*Administrator, error) {
	var a Administrator
	if err := row.Scan(&a.ID, &a.Subject, &a.Email, &a.Name, &a.Picture, &a.CreatedAt, &a.LastLoginAt); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) ListAdministrators(ctx context.Context) ([]*Administrator, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+administratorColumns+` FROM administrators ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list administrators: %w", err)
	}
	defer rows.Close()

	out := []*Administrator{}
	for rows.Next() {
		a, err := scanAdministrator(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) CountAdministrators(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM administrators`).Scan(&n)
	return n, err
}

// GetAdministrator fetches one row, for removal guards.
func (s *Store) GetAdministrator(ctx context.Context, id int64) (*Administrator, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+administratorColumns+` FROM administrators WHERE id = $1`, id)
	return scanAdministrator(row)
}

// AddAdministrator appends an allowlist entry by email. The row exists
// before the person ever signs in; subject binds on their first login.
func (s *Store) AddAdministrator(ctx context.Context, email, name string) (*Administrator, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO administrators (email, name) VALUES ($1, $2) RETURNING id`, email, name).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("administrator %s already exists", email)
		}
		return nil, fmt.Errorf("add administrator: %w", err)
	}
	return s.GetAdministrator(ctx, id)
}

// RemoveAdministrator drops an allowlist entry. Handlers enforce the
// last-administrator and self-removal guards before calling this.
func (s *Store) RemoveAdministrator(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM administrators WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("remove administrator: %w", err)
	}
	return nil
}

// FindAdministratorForLogin resolves OIDC claims to an allowlist row: an
// exact subject match wins; otherwise a row claimed by email that is either
// still unbound (NULL subject) or already bound to this same subject. A row
// whose email matches but whose subject belongs to somebody else does not
// match — that email allowlisted a different identity.
func (s *Store) FindAdministratorForLogin(ctx context.Context, subject, email string) (*Administrator, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+administratorColumns+` FROM administrators WHERE subject = $1`, subject)
	a, err := scanAdministrator(row)
	if err == nil {
		return a, nil
	}
	if err != ErrNotFound {
		return nil, fmt.Errorf("find administrator by subject: %w", err)
	}
	row = s.db.QueryRowContext(ctx,
		`SELECT `+administratorColumns+` FROM administrators
		 WHERE lower(email) = lower($1) AND (subject IS NULL OR subject = $2)`, email, subject)
	return scanAdministrator(row)
}

// UpsertAdministratorLogin binds profile claims + last_login_at onto the
// matched row. Email is left as stored — the allowlist entry is the source
// of truth for identity, the IdP claims only decorate it.
func (s *Store) UpsertAdministratorLogin(ctx context.Context, id int64, subject, name, picture string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE administrators SET
			subject = $2,
			name = CASE WHEN $3 != '' THEN $3 ELSE name END,
			picture = CASE WHEN $4 != '' THEN $4 ELSE picture END,
			last_login_at = now_utc()
		WHERE id = $1`, id, subject, name, picture)
	if err != nil {
		return fmt.Errorf("record administrator login: %w", err)
	}
	return nil
}

// SeedAdministrators inserts one row per email, but only while the table is
// completely empty — the env-derived bootstrap on the admin service's first
// boot. Deleting an administrator in the UI must not be undone by a restart.
// Returns the number of rows inserted.
func (s *Store) SeedAdministrators(ctx context.Context, emails []string) (int, error) {
	if len(emails) == 0 {
		return 0, nil
	}
	n, err := s.CountAdministrators(ctx)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, nil
	}
	inserted := 0
	for _, email := range emails {
		if _, err := s.AddAdministrator(ctx, email, ""); err != nil {
			return inserted, err
		}
		inserted++
	}
	return inserted, nil
}
