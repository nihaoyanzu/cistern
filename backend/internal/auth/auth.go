package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/nihaoyanzu/cistern/internal/store"
)

var ErrInvalidCredentials = errors.New("用户名或密码错误")
var ErrUnauthorized = errors.New("未登录或会话已失效")

const tokenTTL = 24 * time.Hour

type Service struct {
	db     *store.Store
	secret []byte
}

type claims struct {
	Username string `json:"username"`
	jwt.RegisteredClaims
}

// New 使用 JWT 鉴权（无状态，不落库）。secret 优先 CISTERN_JWT_SECRET（兼容 MIRRORHUB_JWT_SECRET），否则持久化到 dataDir/jwt_secret。
func New(db *store.Store, dataDir string) (*Service, error) {
	secret, err := loadOrCreateSecret(dataDir)
	if err != nil {
		return nil, err
	}
	s := &Service{db: db, secret: secret}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hash, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	if err := db.EnsureAdminUser(ctx, "admin", string(hash)); err != nil {
		return nil, err
	}
	return s, nil
}

func loadOrCreateSecret(dataDir string) ([]byte, error) {
	if v := os.Getenv("CISTERN_JWT_SECRET"); v != "" {
		return []byte(v), nil
	}
	if v := os.Getenv("MIRRORHUB_JWT_SECRET"); v != "" {
		return []byte(v), nil
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "jwt_secret")
	if b, err := os.ReadFile(path); err == nil {
		b = []byte(hexNormalize(string(b)))
		if len(b) >= 16 {
			return b, nil
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	encoded := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		return nil, fmt.Errorf("写入 jwt_secret 失败: %w", err)
	}
	return []byte(encoded), nil
}

func hexNormalize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\n' || c == '\r' || c == '\t' {
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

func (s *Service) Login(ctx context.Context, username, password string) (string, error) {
	hash, err := s.db.GetAdminPasswordHash(ctx, username)
	if err != nil {
		return "", ErrInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return "", ErrInvalidCredentials
	}
	now := time.Now()
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   username,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(tokenTTL)),
		},
	})
	return t.SignedString(s.secret)
}

// Logout JWT 无服务端状态，由前端丢弃 token 即可
func (s *Service) Logout(_ string) {}

// ChangePassword 校验旧密码后更新哈希。
func (s *Service) ChangePassword(ctx context.Context, username, oldPassword, newPassword string) error {
	hash, err := s.db.GetAdminPasswordHash(ctx, username)
	if err != nil {
		return ErrInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(oldPassword)) != nil {
		return ErrInvalidCredentials
	}
	next, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return s.db.UpdateAdminPasswordHash(ctx, username, string(next))
}

func (s *Service) Validate(token string) (string, error) {
	if token == "" {
		return "", ErrUnauthorized
	}
	parsed, err := jwt.ParseWithClaims(token, &claims{}, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return s.secret, nil
	})
	if err != nil || !parsed.Valid {
		return "", ErrUnauthorized
	}
	c, ok := parsed.Claims.(*claims)
	if !ok || c.Username == "" {
		return "", ErrUnauthorized
	}
	return c.Username, nil
}
