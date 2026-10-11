package docker

import (
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/livehl/mirrorhub/internal/cache"
	dockerhandler "github.com/livehl/mirrorhub/internal/handlers/docker"
	"github.com/livehl/mirrorhub/internal/platform"
)

const prefetchPriorityDocker = 70

func (p *DockerPlatform) PrefetchPriority() int { return prefetchPriorityDocker }

func (p *DockerPlatform) OwnsPrefetchItem(item string) bool {
	if dockerhandler.IsDockerBlobURL(item) {
		return true
	}
	_, ok := dockerhandler.ParseImageRef(item)
	return ok
}

func (p *DockerPlatform) ExpandPrefetchItem(env platform.PrefetchExpandEnv, item string) ([]string, error) {
	if dockerhandler.IsDockerBlobURL(item) {
		return []string{item}, nil
	}
	ref, ok := dockerhandler.ParseImageRef(item)
	if !ok {
		return nil, fmt.Errorf("无效 docker 镜像引用: %s", item)
	}
	pcfg, ok := env.Cfg.Platforms["docker"]
	if !ok || !pcfg.Enabled {
		return nil, fmt.Errorf("docker 模块未启用")
	}
	reg := strings.TrimRight(strings.TrimSpace(pcfg.Upstream), "/")
	if reg == "" {
		return nil, fmt.Errorf("docker 未配置上游")
	}
	archs := dockerhandler.DefaultTargetArchs(env.Cfg.Scheduler.Prefetch.TargetPlatformList())
	tokens := p.tokens(env.Cfg)
	client := platform.NewOutboundClient(strings.TrimSpace(env.Cfg.Server.UpstreamProxy), 60*time.Second)
	blobs, manifests, err := dockerhandler.ResolveBlobURLs(env.Ctx, client, tokens, reg, ref, archs)
	if err != nil {
		return nil, err
	}
	variants := []string{"list", "v2", "default", "v1"}
	for _, m := range manifests {
		ct := m.ContentType
		if ct == "" {
			ct = "application/vnd.docker.distribution.manifest.v2+json"
		}
		for _, v := range variants {
			key := dockerhandler.ManifestCacheKey(m.SourceURL, v)
			if _, err := env.Backend.PutBytes(key, m.Body, ct, env.Cfg.Cache.IndexTTLSeconds, cache.Meta{
				SourceURL: m.SourceURL,
				Kind:      "index",
			}); err != nil && env.Log != nil {
				env.Log.Warn("docker manifest cache put failed", zap.String("key", key), zap.Error(err))
			}
		}
	}
	if env.Log != nil {
		env.Log.Info("prefetch docker resolved",
			zap.String("repo", ref.Repo),
			zap.String("tag", ref.Tag),
			zap.Int("manifests", len(manifests)),
			zap.Int("blobs", len(blobs)),
		)
	}
	return blobs, nil
}
