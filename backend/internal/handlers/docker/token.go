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
// authBase 为配置的 Auth 上游（如 auth.docker.io）；国内镜像常在 WWW-Authenticate
// 中给出自有 realm（如 docker.1ms.run/openapi/v1/auth/token），DiscoverRealm 会覆盖。
type TokenSource struct {
	client   *http.Client
	authBase string // https://auth.docker.io 或镜像自有 auth 根
	service  string // registry.docker.io / docker.1ms.run
	mu       sync.Mutex
	byScope  map[string]cachedToken
	realmURL string // 完整换票 URL（含 path）；非空时优先于 authBase+"/token"
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// NewTokenSource 创建换票客户端；httpClient 空则用带超时的直连客户端（不读环境变量代理）。
// 生产路径应传入遵守 MirrorHub UpstreamProxy 的客户端。
func NewTokenSource(authBase, service string, httpClient *http.Client) *TokenSource {
	authBase = strings.TrimRight(strings.TrimSpace(authBase), "/")
	service = strings.TrimSpace(service)
	if httpClient == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
		httpClient = &http.Client{Timeout: 30 * time.Second, Transport: tr}
	}
	return &TokenSource{
		client:   httpClient,
		authBase: authBase,
		service:  service,
		byScope:  map[string]cachedToken{},
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

// DiscoverRealm 访问 registry /v2/，从 401 的 WWW-Authenticate 学习换票地址。
// 国内加速站（如 docker.1ms.run）Auth 不在 auth.docker.io，必须走此发现。
func (t *TokenSource) DiscoverRealm(ctx context.Context, registryBase string) error {
	if t == nil {
		return fmt.Errorf("nil token source")
	}
	base := strings.TrimRight(strings.TrimSpace(registryBase), "/")
	if base == "" {
		return fmt.Errorf("empty registry")
	}
	t.mu.Lock()
	if t.realmURL != "" {
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/", nil)
	if err != nil {
		return err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	ch, ok := ParseBearerChallenge(resp.Header)
	if !ok {
		return fmt.Errorf("registry %s: no Bearer challenge (HTTP %d)", base, resp.StatusCode)
	}
	t.ApplyChallenge(ch)
	return nil
}

func (t *TokenSource) tokenEndpointLocked() string {
	if u := strings.TrimSpace(t.realmURL); u != "" {
		return u
	}
	return t.authBase + "/token"
}

// Bearer 返回 Authorization 头可用的 token 字符串（不含 Bearer 前缀）。
func (t *TokenSource) Bearer(ctx context.Context, repo string) (string, error) {
	repo = NormalizeRepo(repo)
	scope := "repository:" + repo + ":pull"
	t.mu.Lock()
	if c, ok := t.byScope[scope]; ok && time.Now().Before(c.expiresAt.Add(-30*time.Second)) {
		tok := c.token
		t.mu.Unlock()
		return tok, nil
	}
	endpoint := t.tokenEndpointLocked()
	service := t.service
	t.mu.Unlock()

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
