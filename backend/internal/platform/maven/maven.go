// Package maven 实现 Maven / Gradle 共用的 Maven2 布局只读缓存平台。
//
// 范围：GET/HEAD 拉取 POM、jar 等制品与 maven-metadata.xml；匿名回源。
// 明确后置：deploy/publish、私仓账号、Plugin Portal、多 upstream 按 path 分流、
// SNAPSHOT 强制刷新策略、Ivy / flatDir。
package maven

import (
	"strings"

	"github.com/nihaoyanzu/cistern/internal/config"
	mavenhandler "github.com/nihaoyanzu/cistern/internal/handlers/maven"
	"github.com/nihaoyanzu/cistern/internal/platform"
	"github.com/nihaoyanzu/cistern/internal/router"
)

func init() {
	platform.Register(&MavenPlatform{})
}

// MavenPlatform 实现 platform.Platform。
type MavenPlatform struct{}

func (p *MavenPlatform) Name() string { return "maven" }

func (p *MavenPlatform) Route(path string, cfg config.PlatformConfig) *router.Match {
	return MatchMaven(path, Routes{
		Upstream:     cfg.Upstream,
		FileUpstream: cfg.FileUpstream,
	})
}

// Routes 上游配置。
type Routes struct {
	Upstream     string
	FileUpstream string
}

// MatchMaven 映射 Maven2 布局路径。
func MatchMaven(path string, r Routes) *router.Match {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if !mavenhandler.IsMavenPath(path) {
		return nil
	}

	up := strings.TrimRight(strings.TrimSpace(r.Upstream), "/")
	fileUp := strings.TrimRight(strings.TrimSpace(r.FileUpstream), "/")
	if up == "" || fileUp == "" {
		return nil
	}

	rel := mavenhandler.StripMaven2Prefix(path)
	if mavenhandler.IsPackagePath(path) {
		return &router.Match{
			Platform:         "maven",
			Strategy:         router.StrategyParallel,
			UpstreamBase:     fileUp,
			TargetURL:        mavenhandler.TargetURL(fileUp, rel),
			ReadOnly:         true,
			SkipUpstreamHead: true,
		}
	}

	return &router.Match{
		Platform:       "maven",
		Strategy:       router.StrategyProxy,
		UpstreamBase:   up,
		TargetURL:      mavenhandler.TargetURL(up, rel),
		IsIndex:        true,
		SmallFileBoost: true,
		ReadOnly:       true,
	}
}

func (p *MavenPlatform) TransformIndex(body []byte, contentType string, cfg config.Config, accept, pageURL string) platform.IndexResult {
	_ = cfg
	_ = accept
	path := ""
	if u := pageURL; u != "" {
		if i := strings.Index(u, "://"); i >= 0 {
			rest := u[i+3:]
			if j := strings.Index(rest, "/"); j >= 0 {
				path = rest[j:]
			}
		}
	}
	ct := mavenhandler.DetectContentType(path, contentType, body)
	return platform.IndexResult{
		Body:        body,
		ETag:        mavenhandler.IndexETag(body),
		ContentType: ct,
	}
}
