package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

type dialect string

const (
	dialectPostgres dialect = "postgres"
	dialectSQLite   dialect = "sqlite"
)

// Store 运营配置存储（默认 SQLite；DATABASE_URL 为 postgres 时用 PostgreSQL）
type Store struct {
	db   *sql.DB
	dial dialect
}

// Open 打开存储。databaseURL 为空时使用 dataDir/cistern.db；若该文件不存在且有 mirrorhub.db，则继续用旧库。
func Open(databaseURL, dataDir string) (*Store, error) {
	url := strings.TrimSpace(databaseURL)
	if url == "" || isSQLiteURL(url) {
		return openSQLite(url, dataDir)
	}
	return openPostgres(url)
}

func isSQLiteURL(url string) bool {
	u := strings.ToLower(url)
	return strings.HasPrefix(u, "sqlite:") ||
		strings.HasPrefix(u, "file:") ||
		strings.HasSuffix(u, ".db") ||
		strings.HasSuffix(u, ".sqlite") ||
		strings.HasSuffix(u, ".sqlite3")
}

func openSQLite(databaseURL, dataDir string) (*Store, error) {
	path := sqlitePath(databaseURL, dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// modernc sqlite DSN
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接 SQLite 失败: %w", err)
	}
	s := &Store{db: db, dial: dialectSQLite}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func sqlitePath(databaseURL, dataDir string) string {
	u := strings.TrimSpace(databaseURL)
	switch {
	case u == "":
		next := filepath.Join(dataDir, "cistern.db")
		prev := filepath.Join(dataDir, "mirrorhub.db")
		if _, err := os.Stat(next); err != nil {
			if _, err := os.Stat(prev); err == nil {
				return prev
			}
		}
		return next
	case strings.HasPrefix(strings.ToLower(u), "sqlite:"):
		return strings.TrimPrefix(u[7:], "//")
	case strings.HasPrefix(strings.ToLower(u), "file:"):
		rest := strings.TrimPrefix(u[5:], "//")
		if i := strings.IndexByte(rest, '?'); i >= 0 {
			rest = rest[:i]
		}
		return rest
	default:
		return u
	}
}

func openPostgres(databaseURL string) (*Store, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	s := &Store{db: db, dial: dialectPostgres}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	var ddl string
	if s.dial == dialectSQLite {
		ddl = `
CREATE TABLE IF NOT EXISTS app_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS admin_users (
    username TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
`
	} else {
		ddl = `
CREATE TABLE IF NOT EXISTS app_settings (
    key TEXT PRIMARY KEY,
    value JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS admin_users (
    username TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`
	}
	_, err := s.db.ExecContext(ctx, ddl)
	return err
}

func (s *Store) EnsureAdminUser(ctx context.Context, username, passwordHash string) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM admin_users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, s.q(`
INSERT INTO admin_users (username, password_hash) VALUES ($1, $2)
ON CONFLICT (username) DO NOTHING
`), username, passwordHash)
	return err
}

func (s *Store) GetAdminPasswordHash(ctx context.Context, username string) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, s.q(`SELECT password_hash FROM admin_users WHERE username = $1`), username).Scan(&hash)
	if err != nil {
		return "", err
	}
	return hash, nil
}

func (s *Store) UpdateAdminPasswordHash(ctx context.Context, username, passwordHash string) error {
	// 占位符按出现顺序绑定：SQLite 的 ? 不认 $n 编号，须与参数顺序一致
	res, err := s.db.ExecContext(ctx, s.q(`
UPDATE admin_users SET password_hash = $1 WHERE username = $2
`), passwordHash, username)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) GetJSON(ctx context.Context, key string, dest any) (bool, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, s.q(`SELECT value FROM app_settings WHERE key = $1`), key).Scan(&raw)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) PutJSON(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var q string
	if s.dial == dialectSQLite {
		q = `
INSERT INTO app_settings (key, value, updated_at)
VALUES (?, ?, datetime('now'))
ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = datetime('now')
`
	} else {
		q = `
INSERT INTO app_settings (key, value, updated_at)
VALUES ($1, $2::jsonb, NOW())
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()
`
	}
	_, err = s.db.ExecContext(ctx, q, key, raw)
	return err
}

// q 将 $1/$2 占位符转为当前方言（SQLite 用 ?）
func (s *Store) q(sql string) string {
	if s.dial != dialectSQLite {
		return sql
	}
	var b strings.Builder
	for i := 0; i < len(sql); i++ {
		if sql[i] == '$' && i+1 < len(sql) && sql[i+1] >= '1' && sql[i+1] <= '9' {
			b.WriteByte('?')
			i++
			for i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9' {
				i++
			}
			continue
		}
		b.WriteByte(sql[i])
	}
	return b.String()
}
