package goproxy

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"go.uber.org/zap"

	"github.com/nihaoyanzu/cistern/internal/cache"
	goproxyhandler "github.com/nihaoyanzu/cistern/internal/handlers/goproxy"
	"github.com/nihaoyanzu/cistern/internal/platform"
)

const prefetchPriorityGoproxy = 80

func (p *GoproxyPlatform) PrefetchPriority() int { return prefetchPriorityGoproxy }

func (p *GoproxyPlatform) OwnsPrefetchItem(item string) bool {
	if goproxyhandler.IsGoproxyArtifactURL(item) {
		return true
	}
	_, ok := goproxyhandler.ParseModuleRef(item)
	return ok
}

func (p *GoproxyPlatform) ExpandPrefetchItem(env platform.PrefetchExpandEnv, item string) ([]string, error) {
	if goproxyhandler.IsGoproxyArtifactURL(item) {
		return []string{item}, nil
	}
	ref, ok := goproxyhandler.ParseModuleRef(item)
	if !ok {
		return nil, fmt.Errorf("无效 goproxy 模块引用: %s", item)
	}
	pcfg, ok := env.Cfg.Platforms["goproxy"]
	if !ok || !pcfg.Enabled {
		return nil, fmt.Errorf("goproxy 模块未启用")
	}
	modUp := strings.TrimRight(strings.TrimSpace(pcfg.Upstream), "/")
	if modUp == "" {
		return nil, fmt.Errorf("goproxy 未配置上游")
	}
	sumUp := strings.TrimRight(strings.TrimSpace(pcfg.MetadataUpstream), "/")
	if sumUp == "" {
		return nil, fmt.Errorf("goproxy 未配置 SumDB 上游")
	}
	infoURL, modURL, zipURL, lookupURL, err := goproxyhandler.ModuleProxyURLs(modUp, sumUp, ref.Path, ref.Version)
	if err != nil {
		return nil, err
	}
	for _, u := range []string{infoURL, lookupURL} {
		if u == "" {
			continue
		}
		status, respHeader, body, getErr := env.Backend.ProxyBytes(env.Ctx, http.MethodGet, u, nil, nil, "goproxy", true)
		if getErr != nil || status < 200 || status >= 300 {
			if env.Log != nil {
				env.Log.Warn("prefetch goproxy index skip", zap.String("url", u), zap.Int("status", status), zap.Error(getErr))
			}
			continue
		}
		ct := respHeader.Get("Content-Type")
		if pathURL, err := url.Parse(u); err == nil {
			ct = goproxyhandler.DetectContentType(pathURL.Path, ct, body)
		}
		key := "goproxy:index:" + cache.KeyFromURL(u)
		if _, err := env.Backend.PutBytes(key, body, ct, env.Cfg.Cache.IndexTTLSeconds, cache.Meta{
			SourceURL:    u,
			Kind:         "index",
			UpstreamETag: respHeader.Get("ETag"),
		}); err != nil && env.Log != nil {
			env.Log.Warn("goproxy index cache put failed", zap.String("key", key), zap.Error(err))
		}
	}
	if env.Log != nil {
		env.Log.Info("prefetch goproxy resolved",
			zap.String("module", ref.Path),
			zap.String("version", ref.Version),
		)
	}
	return []string{modURL, zipURL}, nil
}
