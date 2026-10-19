package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TokenSource 匿名换票，按 repository scope 短时缓存。
// 与 docker 客户端一致：realm/service 只来自上游 401 的 WWW-Authenticate（ApplyChallenge）。
type TokenSource struct {
	client   *http.Client
	service  string // 初始值；ApplyChallenge 可覆盖
	mu       sync.Mutex
	byScope  map[string]cachedToken
	realmURL string // 完整换票 URL（含 path）
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// NewTokenSource 创建换票客户端；httpClient 空则用带超时的直连客户端（不读环境变量代理）。
// 生产路径应传入遵守 Cistern UpstreamProxy 的客户端。
func NewTokenSource(service string, httpClient *http.Client) *TokenSource {
	service = strings.TrimSpace(service)
	if httpClient == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
		httpClient = &http.Client{Timeout: 30 * time.Second, Transport: tr}
	}
	return &TokenSource{
		client:  httpClient,
		service: service,
		byScope: map[string]cachedToken{},
	}
}

// Invalidate 清除某 repo 的缓存 token。
func (t *TokenSource) Invalidate(repo string) {
	scope := "repository:" + NormalizeRepo(repo) + ":pull"
	t.mu.Lock()
	delete(t.byScope, scope)
	t.mu.Unlock()
}

// ApplyChallenge 采用上游 401 的 Bearer 质询（realm/service）。
func (t *TokenSource) ApplyChallenge(ch BearerChallenge) {
	if t == nil || strings.TrimSpace(ch.Realm) == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.realmURL = strings.TrimSpace(ch.Realm)
	if s := strings.TrimSpace(ch.Service); s != "" {
		t.service = s
	}
	// realm 变了，旧票作废
	t.byScope = map[string]cachedToken{}
}

// HasRealm 是否已从 WWW-Authenticate 学到完整换票 URL。
func (t *TokenSource) HasRealm() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(t.realmURL) != ""
}

// Bearer 返回 Authorization 头可用的 token 字符串（不含 Bearer 前缀）。
// 必须先 ApplyChallenge 学到 realm，否则拒绝换票。
func (t *TokenSource) Bearer(ctx context.Context, repo string) (string, error) {
	repo = NormalizeRepo(repo)
	scope := "repository:" + repo + ":pull"
	t.mu.Lock()
	if c, ok := t.byScope[scope]; ok && time.Now().Before(c.expiresAt.Add(-30*time.Second)) {
		tok := c.token
		t.mu.Unlock()
		return tok, nil
	}
	endpoint := strings.TrimSpace(t.realmURL)
	service := t.service
	t.mu.Unlock()

	if endpoint == "" {
		return "", fmt.Errorf("docker auth: missing WWW-Authenticate realm")
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("service", service)
	q.Set("scope", scope)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("docker auth %s: %s", resp.Status, truncate(string(body), 200))
	}
	var parsed struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	tok := parsed.Token
	if tok == "" {
		tok = parsed.AccessToken
	}
	if tok == "" {
		return "", fmt.Errorf("docker auth: empty token")
	}
	exp := 300
	if parsed.ExpiresIn > 0 {
		exp = parsed.ExpiresIn
	}
	t.mu.Lock()
	t.byScope[scope] = cachedToken{token: tok, expiresAt: time.Now().Add(time.Duration(exp) * time.Second)}
	t.mu.Unlock()
	return tok, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
