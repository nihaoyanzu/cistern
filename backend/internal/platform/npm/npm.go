// Package npm 实现 npm registry 平台的 Platform 接口。
//
// 本阶段范围（对齐 Cistern「缓存代理」定位）：
//   - packument / version 元数据缓存 + dist.tarball 改写
//     （abbreviated / full 按 Accept 分键，冷启动走小文档）
//   - tarball 并行/流式下载与落盘缓存（交互冷路径跳过上游 HEAD）
//
// 明确后置（不做）：
//   - local-npm 式全量 skimdb / CouchDB 复制
//   - 私有包 publish / login / OIDC
//   - pnpr 服务端依赖解析加速（/-/pnpr/v0/resolve）
package npm

import (
	"strings"

	"github.com/nihaoyanzu/cistern/internal/config"
	npmhandler "github.com/nihaoyanzu/cistern/internal/handlers/npm"
	"github.com/nihaoyanzu/cistern/internal/platform"
	"github.com/nihaoyanzu/cistern/internal/router"
)

func init() {
	platform.Register(&NPMPlatform{})
}

// NPMPlatform 实现 platform.Platform。
type NPMPlatform struct{}

func (p *NPMPlatform) Name() string { return "npm" }

func (p *NPMPlatform) Route(path string, cfg config.PlatformConfig) *router.Match {
	return MatchNPM(path, Routes{
		Upstream:     cfg.Upstream,
		FileUpstream: cfg.FileUpstream,
	})
}

// Routes npm 上游配置。
type Routes struct {
	Upstream     string
	FileUpstream string
}

// MatchNPM 将本代理路径映射到上游 TargetURL。
func MatchNPM(path string, r Routes) *router.Match {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if npmhandler.IsReservedProxyPath(path) {
		return nil
	}
	metaUp := strings.TrimRight(strings.TrimSpace(r.Upstream), "/")
	fileUp := strings.TrimRight(strings.TrimSpace(r.FileUpstream), "/")
	if metaUp == "" || fileUp == "" {
		return nil
	}

	if npmhandler.IsTarballPath(path) {
		return &router.Match{
			Platform:         "npm",
			Strategy:         router.StrategyParallel,
			UpstreamBase:     fileUp,
			TargetURL:        fileUp + path,
			ReadOnly:         true,
			SkipUpstreamHead: true,
		}
	}

	// packument、version 文档、/-/ 元数据端点等一律当索引缓存
	return &router.Match{
		Platform:       "npm",
		Strategy:       router.StrategyProxy,
		UpstreamBase:   metaUp,
		TargetURL:      metaUp + path,
		IsIndex:        true,
		SmallFileBoost: true,
		ReadOnly:       true,
	}
}

func (p *NPMPlatform) TransformIndex(body []byte, contentType string, cfg config.Config, accept, pageURL string) platform.IndexResult {
	_ = pageURL
	out := npmhandler.RewritePackument(body, cfg)
	proxyBase := npmhandler.PublicBaseURL(cfg)
	ct := npmhandler.DetectJSONContentType(contentType, accept, out)
	return platform.IndexResult{
		Body:        out,
		ETag:        npmhandler.IndexETag(out, proxyBase),
		ContentType: ct,
	}
}
