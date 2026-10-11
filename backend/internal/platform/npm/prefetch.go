package npm

import (
	"fmt"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/livehl/mirrorhub/internal/cache"
	npmhandler "github.com/livehl/mirrorhub/internal/handlers/npm"
	"github.com/livehl/mirrorhub/internal/platform"
)

const prefetchPriorityNPM = 60

func (p *NPMPlatform) PrefetchPriority() int { return prefetchPriorityNPM }

func (p *NPMPlatform) OwnsPrefetchItem(item string) bool {
	if npmhandler.IsTarballURL(item) {
		return true
	}
	_, ok := npmhandler.ParsePackageSpec(item)
	return ok
}

func (p *NPMPlatform) ExpandPrefetchItem(env platform.PrefetchExpandEnv, item string) ([]string, error) {
	pcfg, ok := env.Cfg.Platforms["npm"]
	if !ok || !pcfg.Enabled {
		return nil, fmt.Errorf("npm 模块未启用")
	}
	if npmhandler.IsTarballURL(item) {
		return []string{item}, nil
	}
	root, ok := npmhandler.ParsePackageSpec(item)
	if !ok {
		return nil, fmt.Errorf("无效 npm 规格: %s", item)
	}
	metaUp := strings.TrimRight(strings.TrimSpace(pcfg.Upstream), "/")
	if metaUp == "" {
		return nil, fmt.Errorf("npm 未配置上游")
	}

	type node struct {
		ref npmhandler.PackageRef
	}
	queue := []node{{ref: root}}
	seenPkg := map[string]struct{}{}
	var allURLs []string
	urlSeen := map[string]struct{}{}

	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		name := n.ref.Name
		if _, dup := seenPkg[name]; dup {
			continue
		}
		seenPkg[name] = struct{}{}

		packURL := npmhandler.PackumentURL(metaUp, name)
		if packURL == "" {
			continue
		}
		status, respHeader, body, getErr := env.Backend.ProxyBytes(env.Ctx, http.MethodGet, packURL, nil, nil, "npm", true)
		if getErr != nil || status < 200 || status >= 300 {
			if env.Log != nil {
				env.Log.Warn("prefetch npm packument skip",
					zap.String("pkg", name),
					zap.String("url", packURL),
					zap.Int("status", status),
					zap.Error(getErr),
				)
			}
			continue
		}
		ct := npmhandler.DetectJSONContentType(respHeader.Get("Content-Type"), "", body)
		key := "npm:index:" + cache.KeyFromURL(packURL)
		if _, err := env.Backend.PutBytes(key, body, ct, env.Cfg.Cache.IndexTTLSeconds, cache.Meta{
			SourceURL:    packURL,
			Kind:         "index",
			UpstreamETag: respHeader.Get("ETag"),
		}); err != nil && env.Log != nil {
			env.Log.Warn("npm packument cache put failed", zap.String("key", key), zap.Error(err))
		}

		tb, ver, deps, err := npmhandler.ResolvePackumentTarball(body, n.ref.Version)
		if err != nil {
			if env.Log != nil {
				env.Log.Warn("prefetch npm resolve skip",
					zap.String("pkg", name),
					zap.String("spec", n.ref.Version),
					zap.Error(err),
				)
			}
			continue
		}
		_ = ver
		tb = strings.TrimSpace(tb)
		if tb != "" {
			if _, dup := urlSeen[tb]; !dup {
				urlSeen[tb] = struct{}{}
				allURLs = append(allURLs, tb)
			}
		}
		for depName, depSpec := range deps {
			depName = strings.TrimSpace(depName)
			depSpec = strings.TrimSpace(depSpec)
			if depName == "" || depSpec == "" {
				continue
			}
			if _, dup := seenPkg[depName]; dup {
				continue
			}
			if !npmhandler.IsRegistryVersionSpec(depSpec) {
				continue
			}
			queue = append(queue, node{ref: npmhandler.PackageRef{Name: depName, Version: depSpec}})
		}
	}

	if len(allURLs) == 0 {
		return nil, fmt.Errorf("未能从 npm 解析出可下载 tarball: %s", item)
	}
	if env.Log != nil {
		env.Log.Info("prefetch npm resolved",
			zap.String("root", root.Spec()),
			zap.Int("packages", len(seenPkg)),
			zap.Int("tarballs", len(allURLs)),
		)
	}
	return allURLs, nil
}
