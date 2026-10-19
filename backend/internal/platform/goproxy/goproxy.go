// Package goproxy 实现 Go module proxy（GOPROXY）只读缓存平台。
//
// 范围：模块 list/info/mod/zip + sumdb 代理；预取 go.mod/go.sum。
// 明确后置：私有模块鉴权、自建 sumdb、VCS direct 代抓。
package goproxy

import (
	"strings"

	"github.com/nihaoyanzu/cistern/internal/config"
	goproxyhandler "github.com/nihaoyanzu/cistern/internal/handlers/goproxy"
	"github.com/nihaoyanzu/cistern/internal/platform"
	"github.com/nihaoyanzu/cistern/internal/router"
)

func init() {
	platform.Register(&GoproxyPlatform{})
}

// GoproxyPlatform 实现 platform.Platform。
type GoproxyPlatform struct{}

func (p *GoproxyPlatform) Name() string { return "goproxy" }

func (p *GoproxyPlatform) Route(path string, cfg config.PlatformConfig) *router.Match {
	return MatchGoproxy(path, Routes{
		Upstream:     cfg.Upstream,
		FileUpstream: cfg.FileUpstream,
		SumUpstream:  cfg.MetadataUpstream,
	})
}

// Routes 上游配置。
type Routes struct {
	Upstream     string
	FileUpstream string
	SumUpstream  string
}

// MatchGoproxy 映射 GOPROXY / sumdb 路径。supported 由 proxy 短路。
func MatchGoproxy(path string, r Routes) *router.Match {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if goproxyhandler.IsSumDBSupported(path) {
		return nil
	}
	if !goproxyhandler.IsGoproxyPath(path) {
		return nil
	}

	modUp := strings.TrimRight(strings.TrimSpace(r.Upstream), "/")
	fileUp := strings.TrimRight(strings.TrimSpace(r.FileUpstream), "/")
	sumUp := strings.TrimRight(strings.TrimSpace(r.SumUpstream), "/")
	if modUp == "" || fileUp == "" {
		return nil
	}

	if goproxyhandler.IsSumDBPath(path) {
		target := goproxyhandler.SumDBFetchURL(sumUp, path)
		if target == "" {
			return nil
		}
		return &router.Match{
			Platform:       "goproxy",
			Strategy:       router.StrategyProxy,
			UpstreamBase:   sumUp,
			TargetURL:      target,
			IsIndex:        true,
			SmallFileBoost: true,
			ReadOnly:       true,
		}
	}

	if goproxyhandler.IsPackagePath(path) {
		return &router.Match{
			Platform:         "goproxy",
			Strategy:         router.StrategyParallel,
			UpstreamBase:     fileUp,
			TargetURL:        fileUp + path,
			ReadOnly:         true,
			SkipUpstreamHead: true,
		}
	}

	return &router.Match{
		Platform:       "goproxy",
		Strategy:       router.StrategyProxy,
		UpstreamBase:   modUp,
		TargetURL:      modUp + path,
		IsIndex:        true,
		SmallFileBoost: true,
		ReadOnly:       true,
	}
}

func (p *GoproxyPlatform) TransformIndex(body []byte, contentType string, cfg config.Config, accept, pageURL string) platform.IndexResult {
	_ = cfg
	_ = accept
	path := ""
	if u := pageURL; u != "" {
		// pageURL 可能是完整上游 URL，取 path 后缀用于 MIME
		if i := strings.Index(u, "://"); i >= 0 {
			rest := u[i+3:]
			if j := strings.Index(rest, "/"); j >= 0 {
				path = rest[j:]
			}
		}
	}
	ct := goproxyhandler.DetectContentType(path, contentType, body)
	return platform.IndexResult{
		Body:        body,
		ETag:        goproxyhandler.IndexETag(body),
		ContentType: ct,
	}
}
