package maven

import (
	"fmt"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/livehl/mirrorhub/internal/cache"
	mavenhandler "github.com/livehl/mirrorhub/internal/handlers/maven"
	"github.com/livehl/mirrorhub/internal/platform"
)

const textDetectPriorityMaven = 70

func (p *MavenPlatform) TextDetectPriority() int { return textDetectPriorityMaven }

func (p *MavenPlatform) LookLikePrefetchText(text string) bool {
	return mavenhandler.LookLikePom(text) ||
		mavenhandler.LookLikeLibsVersionsTOML(text) ||
		mavenhandler.LookLikeBuildGradle(text) ||
		mavenhandler.LookLikeGradleProperties(text) ||
		mavenhandler.LookLikeGAVList(text)
}

// ResolvePrefetchText 粘贴 POM 时回源 parent/BOM 补全无 version 依赖。
func (p *MavenPlatform) ResolvePrefetchText(env platform.PrefetchExpandEnv, text string) (platform.TextDetectResult, bool) {
	if mavenhandler.LookLikePom(text) {
		coords, skip := mavenhandler.ResolvePomDependencies(text, pomFetchFromEnv(env))
		items := make([]string, 0, len(coords))
		for _, c := range coords {
			items = append(items, c.String())
		}
		if len(items) == 0 {
			return platform.TextDetectResult{Skipped: skip, Kind: "maven_pom"}, false
		}
		return platform.TextDetectResult{Items: items, Skipped: skip, Kind: "maven_pom"}, true
	}
	return p.ParsePrefetchText(text)
}

func (p *MavenPlatform) ParsePrefetchText(text string) (platform.TextDetectResult, bool) {
	if mavenhandler.LookLikePom(text) {
		coords, skip := mavenhandler.ParsePomDependencies(text)
		items := make([]string, 0, len(coords))
		for _, c := range coords {
			items = append(items, c.String())
		}
		if len(items) == 0 {
			return platform.TextDetectResult{Skipped: skip, Kind: "maven_pom"}, false
		}
		return platform.TextDetectResult{Items: items, Skipped: skip, Kind: "maven_pom"}, true
	}
	if mavenhandler.LookLikeLibsVersionsTOML(text) {
		coords, skip := mavenhandler.ParseLibsVersionsTOML(text)
		items := make([]string, 0, len(coords))
		for _, c := range coords {
			items = append(items, c.String())
		}
		if len(items) == 0 {
			return platform.TextDetectResult{Skipped: skip, Kind: "maven_libs_versions"}, false
		}
		return platform.TextDetectResult{Items: items, Skipped: skip, Kind: "maven_libs_versions"}, true
	}
	if mavenhandler.LookLikeBuildGradle(text) {
		coords, skip := mavenhandler.ParseBuildGradle(text)
		items := make([]string, 0, len(coords))
		for _, c := range coords {
			items = append(items, c.String())
		}
		if len(items) == 0 {
			return platform.TextDetectResult{Skipped: skip, Kind: "maven_build_gradle"}, false
		}
		return platform.TextDetectResult{Items: items, Skipped: skip, Kind: "maven_build_gradle"}, true
	}
	if mavenhandler.LookLikeGradleProperties(text) {
		coords, skip := mavenhandler.ParseGradleProperties(text)
		items := make([]string, 0, len(coords))
		for _, c := range coords {
			items = append(items, c.String())
		}
		if len(items) == 0 {
			// 识别为 properties，但无可预取 GAV（常见于 Spring 的 xxxVersion=…）
			return platform.TextDetectResult{Skipped: skip, Kind: "maven_gradle_props"}, false
		}
		return platform.TextDetectResult{Items: items, Skipped: skip, Kind: "maven_gradle_props"}, true
	}
	if mavenhandler.LookLikeGAVList(text) {
		coords, skip := mavenhandler.ParseGAVList(text)
		items := make([]string, 0, len(coords))
		for _, c := range coords {
			items = append(items, c.String())
		}
		if len(items) == 0 {
			return platform.TextDetectResult{Skipped: skip, Kind: "maven_gav"}, false
		}
		return platform.TextDetectResult{Items: items, Skipped: skip, Kind: "maven_gav"}, true
	}
	return platform.TextDetectResult{}, false
}

// pomFetchFromEnv 构造回源拉取 POM 的回调；上游未配置时返回 nil（仅本地解析）。
func pomFetchFromEnv(env platform.PrefetchExpandEnv) mavenhandler.PomFetch {
	if env.Backend == nil {
		return nil
	}
	pcfg, ok := env.Cfg.Platforms["maven"]
	if !ok || !pcfg.Enabled {
		return nil
	}
	up := strings.TrimRight(strings.TrimSpace(pcfg.FileUpstream), "/")
	if up == "" {
		return nil
	}
	return func(groupID, artifactID, version string) ([]byte, error) {
		pomURL := mavenhandler.ArtifactURL(up, groupID, artifactID, version, "pom")
		status, respHeader, body, err := env.Backend.ProxyBytes(env.Ctx, http.MethodGet, pomURL, nil, nil, "maven", true)
		if err != nil {
			return nil, err
		}
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("HTTP %d", status)
		}
		ct := mavenhandler.DetectContentType("/.pom", respHeader.Get("Content-Type"), body)
		pkgKey := "maven:pkg:" + cache.KeyFromURL(pomURL)
		if _, putErr := env.Backend.PutBytes(pkgKey, body, ct, env.Cfg.Cache.PackageTTLSeconds, cache.Meta{
			SourceURL:    pomURL,
			Kind:         "package",
			UpstreamETag: respHeader.Get("ETag"),
		}); putErr != nil && env.Log != nil {
			env.Log.Warn("maven parent pom cache put failed", zap.String("key", pkgKey), zap.Error(putErr))
		}
		return body, nil
	}
}
