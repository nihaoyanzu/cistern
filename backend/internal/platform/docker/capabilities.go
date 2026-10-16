package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/livehl/mirrorhub/internal/config"
	dockerhandler "github.com/livehl/mirrorhub/internal/handlers/docker"
	"github.com/livehl/mirrorhub/internal/platform"
)

// DockerPlatform 实现 platform.Platform 及可选能力。
type DockerPlatform struct {
	authMu sync.Mutex
	src    *dockerhandler.TokenSource
	svc    string
	proxy  string
}

func (p *DockerPlatform) tokens(cfg config.Config) *dockerhandler.TokenSource {
	pcfg := cfg.Platforms["docker"]
	svc := dockerhandler.AuthServiceFromRegistry(pcfg.Upstream)
	proxy := strings.TrimSpace(cfg.Server.UpstreamProxy)
	p.authMu.Lock()
	defer p.authMu.Unlock()
	if p.src != nil && p.svc == svc && p.proxy == proxy {
		return p.src
	}
	// 换票必须走与下载器一致的出站代理
	client := platform.NewOutboundClient(proxy, 30*time.Second)
	p.src = dockerhandler.NewTokenSource(svc, client)
	p.svc = svc
	p.proxy = proxy
	return p.src
}

func (p *DockerPlatform) TryLocalProbe(w http.ResponseWriter, r *http.Request, cfg config.Config) bool {
	if !dockerhandler.IsV2Root(r.URL.Path) {
		return false
	}
	pcfg, ok := cfg.Platforms["docker"]
	if !ok || !pcfg.Enabled {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "docker mirror is read-only", http.StatusMethodNotAllowed)
	}
	return true
}

func (p *DockerPlatform) InjectUpstreamAuth(ctx context.Context, headers http.Header, cfg config.Config, reqPath string) error {
	repo := ""
	if rp, _, ok := dockerhandler.ParseManifestPath(reqPath); ok {
		repo = rp
	} else if rp, _, ok := dockerhandler.ParseBlobPath(reqPath); ok {
		repo = rp
	} else {
		return nil
	}
	src := p.tokens(cfg)
	// 无 realm 时不预注入：先匿名试探，401 后再 ApplyChallenge（对齐 docker 客户端）
	if !src.HasRealm() {
		return nil
	}
	tok, err := src.Bearer(ctx, repo)
	if err != nil {
		return err
	}
	headers.Set("Authorization", "Bearer "+tok)
	return nil
}

func (p *DockerPlatform) InvalidateUpstreamAuth(cfg config.Config, reqPath string, challenge http.Header) {
	repo := ""
	if rp, _, ok := dockerhandler.ParseManifestPath(reqPath); ok {
		repo = rp
	} else if rp, _, ok := dockerhandler.ParseBlobPath(reqPath); ok {
		repo = rp
	}
	src := p.tokens(cfg)
	if ch, ok := dockerhandler.ParseBearerChallenge(challenge); ok {
		src.ApplyChallenge(ch) // 已清空缓存票
		return
	}
	if repo != "" {
		src.Invalidate(repo)
	}
}

func (p *DockerPlatform) IndexCacheVariant(accept string) string {
	return dockerhandler.ManifestAcceptVariant(accept)
}

func (p *DockerPlatform) PrepareIndexHeaders(_ string, headers http.Header) {
	if strings.TrimSpace(headers.Get("Accept")) == "" {
		headers.Set("Accept", dockerhandler.DefaultManifestAccept())
	}
}

func (p *DockerPlatform) DecorateIndexHeaders(w http.ResponseWriter, body []byte) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if len(body) == 0 {
		return
	}
	w.Header().Del("Docker-Content-Digest")
	sum := sha256.Sum256(body)
	w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(sum[:]))
}

func (p *DockerPlatform) DecoratePackageHeaders(w http.ResponseWriter) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
}

func (p *DockerPlatform) PackageCacheIdentity(reqPath, _ string, _ http.Header, defaultKey string) (string, string) {
	if _, dig, ok := dockerhandler.ParseBlobPath(reqPath); ok {
		hexDig := strings.TrimPrefix(dig, "sha256:")
		return dockerhandler.BlobCacheKey(dig), hexDig
	}
	return defaultKey, ""
}

func (p *DockerPlatform) DetectIndexContentType(upstreamCT, _ string, body []byte) string {
	return dockerhandler.DetectManifestContentType(upstreamCT, body)
}
