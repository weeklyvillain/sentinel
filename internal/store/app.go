package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Installation is a GitHub App installation.
type Installation struct {
	ID            int64     `json:"id"`
	Account       string    `json:"account"`
	AccountType   string    `json:"account_type"`
	RepoSelection string    `json:"repo_selection"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Repo is a repository an installation grants access to. Allowed means the
// dashboard user authorized Sentinel to file issues there.
type Repo struct {
	FullName       string `json:"full_name"`
	InstallationID int64  `json:"installation_id"`
	Private        bool   `json:"private"`
	Allowed        bool   `json:"allowed"`
}

// SyncInstallation replaces an installation's repo list, keeping the allowed
// flag of repos that are still present.
func (s *Store) SyncInstallation(ctx context.Context, inst Installation, repos []Repo) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := ms(time.Now())
	if _, err := tx.ExecContext(ctx, `INSERT INTO gh_installations (id, account, account_type, repo_selection, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET account = excluded.account, account_type = excluded.account_type,
		repo_selection = excluded.repo_selection, updated_at = excluded.updated_at`,
		inst.ID, inst.Account, inst.AccountType, inst.RepoSelection, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS sync_keep (full_name TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	tx.ExecContext(ctx, `DELETE FROM sync_keep`)
	for _, r := range repos {
		if _, err := tx.ExecContext(ctx, `INSERT INTO gh_repos (full_name, installation_id, private, allowed, updated_at) VALUES (?, ?, ?, 0, ?)
			ON CONFLICT(full_name) DO UPDATE SET installation_id = excluded.installation_id, private = excluded.private, updated_at = excluded.updated_at`,
			r.FullName, inst.ID, boolInt(r.Private), now); err != nil {
			return err
		}
		tx.ExecContext(ctx, `INSERT OR IGNORE INTO sync_keep (full_name) VALUES (?)`, r.FullName)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gh_repos WHERE installation_id = ? AND full_name NOT IN (SELECT full_name FROM sync_keep)`, inst.ID); err != nil {
		return err
	}
	tx.ExecContext(ctx, `DELETE FROM sync_keep`)
	return tx.Commit()
}

// RemoveInstallation forgets an installation and its repos.
func (s *Store) RemoveInstallation(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM gh_repos WHERE installation_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM gh_installations WHERE id = ?`, id)
	return err
}

// ListInstallations returns stored installations.
func (s *Store) ListInstallations(ctx context.Context) ([]Installation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, account, account_type, repo_selection, updated_at FROM gh_installations ORDER BY account`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Installation{}
	for rows.Next() {
		var i Installation
		var u int64
		if err := rows.Scan(&i.ID, &i.Account, &i.AccountType, &i.RepoSelection, &u); err != nil {
			return nil, err
		}
		i.UpdatedAt = fromMs(u)
		out = append(out, i)
	}
	return out, rows.Err()
}

// ListRepos returns all repos the app can see.
func (s *Store) ListRepos(ctx context.Context) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT full_name, installation_id, private, allowed FROM gh_repos ORDER BY full_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Repo{}
	for rows.Next() {
		var r Repo
		var p, a int
		if err := rows.Scan(&r.FullName, &r.InstallationID, &p, &a); err != nil {
			return nil, err
		}
		r.Private, r.Allowed = p == 1, a == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRepo returns one repo.
func (s *Store) GetRepo(ctx context.Context, fullName string) (Repo, error) {
	var r Repo
	var p, a int
	err := s.db.QueryRowContext(ctx, `SELECT full_name, installation_id, private, allowed FROM gh_repos WHERE full_name = ? COLLATE NOCASE`, fullName).
		Scan(&r.FullName, &r.InstallationID, &p, &a)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	r.Private, r.Allowed = p == 1, a == 1
	return r, err
}

// SetRepoAllowed authorizes (or revokes) issue creation in a repo.
func (s *Store) SetRepoAllowed(ctx context.Context, fullName string, allowed bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE gh_repos SET allowed = ? WHERE full_name = ? COLLATE NOCASE`, boolInt(allowed), fullName)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Setting returns a setting value ("" if unset).
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting stores a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Session is a signed-in dashboard user.
type Session struct {
	ID        string    `json:"-"`
	Login     string    `json:"login"`
	Name      string    `json:"name,omitempty"`
	AvatarURL string    `json:"avatar_url,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CreateSession stores a session keyed by the hash of its token.
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, login, name, avatar_url, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.Login, sess.Name, sess.AvatarURL, ms(time.Now()), ms(sess.ExpiresAt))
	return err
}

// GetSession returns a non-expired session.
func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	var sess Session
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT id, login, name, avatar_url, expires_at FROM sessions WHERE id = ? AND expires_at > ?`, id, ms(time.Now())).
		Scan(&sess.ID, &sess.Login, &sess.Name, &sess.AvatarURL, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return sess, ErrNotFound
	}
	sess.ExpiresAt = fromMs(exp)
	return sess, err
}

// DeleteSession signs a session out.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
