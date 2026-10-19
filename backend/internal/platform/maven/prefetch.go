package maven

import (
	"fmt"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/nihaoyanzu/cistern/internal/cache"
	mavenhandler "github.com/nihaoyanzu/cistern/internal/handlers/maven"
	"github.com/nihaoyanzu/cistern/internal/platform"
)

const prefetchPriorityMaven = 90

func (p *MavenPlatform) PrefetchPriority() int { return prefetchPriorityMaven }

func (p *MavenPlatform) OwnsPrefetchItem(item string) bool {
	if mavenhandler.IsMavenArtifactURL(item) {
		return true
	}
	_, ok := mavenhandler.ParseCoordinate(item)
	return ok
}

func (p *MavenPlatform) ExpandPrefetchItem(env platform.PrefetchExpandEnv, item string) ([]string, error) {
	if mavenhandler.IsMavenArtifactURL(item) {
		return []string{item}, nil
	}
	root, ok := mavenhandler.ParseCoordinate(item)
	if !ok {
		return nil, fmt.Errorf("无效 maven 坐标: %s", item)
	}
	pcfg, ok := env.Cfg.Platforms["maven"]
	if !ok || !pcfg.Enabled {
		return nil, fmt.Errorf("maven 模块未启用")
	}
	up := strings.TrimRight(strings.TrimSpace(pcfg.FileUpstream), "/")
	if up == "" {
		return nil, fmt.Errorf("maven 未配置上游")
	}
	type node struct {
		c mavenhandler.Coordinate
	}
	queue := []node{{c: root}}
	seen := map[string]struct{}{}
	var allURLs []string
	urlSeen := map[string]struct{}{}

	addURL := func(u string) {
		if u == "" {
			return
		}
		if _, ok := urlSeen[u]; ok {
			return
		}
		urlSeen[u] = struct{}{}
		allURLs = append(allURLs, u)
	}

	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		key := n.c.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		pomURL := mavenhandler.ArtifactURL(up, n.c.GroupID, n.c.ArtifactID, n.c.Version, "pom")
		addURL(pomURL)
		pack := strings.ToLower(strings.TrimSpace(n.c.Packaging))
		if pack == "" {
			pack = "jar"
		}
		if pack != "pom" &&
			!mavenhandler.SkipPrefetchPackaging(pack) &&
			!mavenhandler.SkipPrefetchClassifier(n.c.Classifier) {
			addURL(mavenhandler.ArtifactURLWithClassifier(up, n.c.GroupID, n.c.ArtifactID, n.c.Version, pack, n.c.Classifier))
		}

		status, respHeader, body, getErr := env.Backend.ProxyBytes(env.Ctx, http.MethodGet, pomURL, nil, nil, "maven", true)
		if getErr != nil || status < 200 || status >= 300 {
			if env.Log != nil {
				env.Log.Warn("prefetch maven pom skip", zap.String("url", pomURL), zap.Int("status", status), zap.Error(getErr))
			}
			continue
		}
		ct := mavenhandler.DetectContentType("/.pom", respHeader.Get("Content-Type"), body)
		pkgKey := "maven:pkg:" + cache.KeyFromURL(pomURL)
		if _, err := env.Backend.PutBytes(pkgKey, body, ct, env.Cfg.Cache.PackageTTLSeconds, cache.Meta{
			SourceURL:    pomURL,
			Kind:         "package",
			UpstreamETag: respHeader.Get("ETag"),
		}); err != nil && env.Log != nil {
			env.Log.Warn("maven pom cache put failed", zap.String("key", pkgKey), zap.Error(err))
		}

		deps, _ := mavenhandler.ResolvePomDependencies(string(body), pomFetchFromEnv(env))
		for _, d := range deps {
			scope := strings.ToLower(strings.TrimSpace(d.Scope))
			pack := strings.ToLower(strings.TrimSpace(d.Packaging))
			// 允许 import BOM / pom 包装；其余仅 compile/runtime/空 scope；跳过 test-jar / sources 等
			if mavenhandler.SkipPrefetchPackaging(pack) || mavenhandler.SkipPrefetchClassifier(d.Classifier) {
				continue
			}
			if scope == "import" || pack == "pom" {
				// ok
			} else if scope != "" && scope != "compile" && scope != "runtime" {
				continue
			}
			dk := d.String()
			if _, dup := seen[dk]; dup {
				continue
			}
			queue = append(queue, node{c: d})
		}
	}

	if env.Log != nil {
		env.Log.Info("prefetch maven resolved",
			zap.String("root", root.String()),
			zap.Int("coords", len(seen)),
			zap.Int("files", len(allURLs)),
		)
	}
	return allURLs, nil
}
