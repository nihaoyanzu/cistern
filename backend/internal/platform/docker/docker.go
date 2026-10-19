// Package docker 实现 Docker Hub / Registry V2 只读缓存平台。
//
// 范围：pull-through 缓存 + 预取 image 列表；离线依赖已缓存 manifest(STALE) 与 blob。
// 明确后置：登录、push、私有仓库、多 registry 路由。
package docker

import (
	"strings"

	"github.com/nihaoyanzu/cistern/internal/config"
	dockerhandler "github.com/nihaoyanzu/cistern/internal/handlers/docker"
	"github.com/nihaoyanzu/cistern/internal/platform"
	"github.com/nihaoyanzu/cistern/internal/router"
)

func init() {
	platform.Register(&DockerPlatform{})
}

func (p *DockerPlatform) Name() string { return "docker" }

func (p *DockerPlatform) Route(path string, cfg config.PlatformConfig) *router.Match {
	return MatchDocker(path, Routes{
		Upstream:     cfg.Upstream,
		FileUpstream: cfg.FileUpstream,
	})
}

// Routes 上游配置。
type Routes struct {
	Upstream     string
	FileUpstream string
}

// MatchDocker 映射 /v2 路径。探活 /v2/ 由 proxy 短路，此处返回 nil。
func MatchDocker(path string, r Routes) *router.Match {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if dockerhandler.IsV2Root(path) {
		return nil
	}
	if !dockerhandler.IsRegistryPath(path) {
		return nil
	}
	reg := strings.TrimRight(strings.TrimSpace(r.Upstream), "/")
	fileUp := strings.TrimRight(strings.TrimSpace(r.FileUpstream), "/")
	if reg == "" || fileUp == "" {
		return nil
	}

	if dockerhandler.IsBlobPath(path) {
		// 必须先探测真实 size：containerd 用 descriptor/HEAD Content-Length 校验，禁止 SkipUpstreamHead
		return &router.Match{
			Platform:     "docker",
			Strategy:     router.StrategyParallel,
			UpstreamBase: fileUp,
			TargetURL:    fileUp + path,
			ReadOnly:     true,
		}
	}
	if dockerhandler.IsManifestPath(path) {
		return &router.Match{
			Platform:       "docker",
			Strategy:       router.StrategyProxy,
			UpstreamBase:   reg,
			TargetURL:      reg + path,
			IsIndex:        true,
			SmallFileBoost: true,
			ReadOnly:       true,
		}
	}
	// 其它 /v2/... 只读透传为索引（少见）
	return &router.Match{
		Platform:       "docker",
		Strategy:       router.StrategyProxy,
		UpstreamBase:   reg,
		TargetURL:      reg + path,
		IsIndex:        true,
		SmallFileBoost: true,
		ReadOnly:       true,
	}
}

func (p *DockerPlatform) TransformIndex(body []byte, contentType string, cfg config.Config, accept, pageURL string) platform.IndexResult {
	_ = cfg
	_ = accept
	_ = pageURL
	ct := dockerhandler.DetectManifestContentType(contentType, body)
	return platform.IndexResult{
		Body:        body,
		ETag:        dockerhandler.IndexETag(body),
		ContentType: ct,
	}
}
