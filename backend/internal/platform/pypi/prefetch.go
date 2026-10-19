package pypi

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"go.uber.org/zap"

	"github.com/nihaoyanzu/cistern/internal/config"
	pypihandler "github.com/nihaoyanzu/cistern/internal/handlers/pypi"
	"github.com/nihaoyanzu/cistern/internal/platform"
)

// 最低优先级：兜底 PEP 508 规格。
const prefetchPriorityPyPI = 0

func (p *PyPIPlatform) PrefetchPriority() int { return prefetchPriorityPyPI }

func (p *PyPIPlatform) OwnsPrefetchItem(item string) bool {
	if strings.HasPrefix(item, "http://") || strings.HasPrefix(item, "https://") {
		return false
	}
	req, err := pypihandler.ParseRequirement(item)
	if err != nil {
		return false
	}
	name := pypihandler.NormalizeName(req.Name)
	return pypihandler.IsPlausiblePackageName(name)
}

func (p *PyPIPlatform) ExpandPrefetchItem(env platform.PrefetchExpandEnv, item string) ([]string, error) {
	pcfg, ok := env.Cfg.Platforms["pypi"]
	if !ok || !pcfg.Enabled {
		return nil, fmt.Errorf("pypi 模块未启用")
	}
	pf := env.Cfg.Scheduler.Prefetch
	plats := pf.TargetPlatformList()
	targetEnv := pypihandler.TargetEnv{Python: pf.TargetPythonVersions(), Platforms: plats}

	type node struct {
		spec  string
		depth int
	}
	queue := []node{{spec: item, depth: 0}}
	seenPkg := map[string]struct{}{}
	var allURLs []string
	urlSeen := map[string]struct{}{}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		req, err := pypihandler.ParseRequirement(cur.spec)
		if err != nil {
			if cur.depth == 0 {
				return nil, err
			}
			continue
		}
		name := pypihandler.NormalizeName(req.Name)
		if !pypihandler.IsPlausiblePackageName(name) {
			if cur.depth == 0 {
				return nil, fmt.Errorf("无效或非包名: %s", req.Raw)
			}
			continue
		}
		if _, ok := seenPkg[name]; ok {
			continue
		}
		seenPkg[name] = struct{}{}

		files, metaURL, err := resolvePackageFiles(env, req, pf)
		if err != nil {
			if cur.depth == 0 {
				return nil, err
			}
			if env.Log != nil {
				env.Log.Warn("prefetch dep resolve skip", zap.String("spec", cur.spec), zap.Error(err))
			}
			continue
		}
		for _, u := range files {
			if _, ok := urlSeen[u]; ok {
				continue
			}
			urlSeen[u] = struct{}{}
			allURLs = append(allURLs, u)
		}

		if metaURL == "" {
			if env.Log != nil {
				env.Log.Warn("prefetch skip deps: no metadata URL",
					zap.String("spec", cur.spec),
					zap.Int("files", len(files)),
					zap.Int("depth", cur.depth),
				)
			}
			continue
		}
		deps, err := fetchRequires(env, metaURL)
		if err != nil {
			if env.Log != nil {
				env.Log.Warn("prefetch requires skip", zap.String("meta", metaURL), zap.Error(err))
			}
			continue
		}
		for _, dep := range deps {
			reqPart, marker := pypihandler.SplitReqMarker(dep)
			if !pypihandler.EvalMarker(marker, targetEnv) {
				continue
			}
			if _, err := pypihandler.ParseRequirement(reqPart); err != nil {
				continue
			}
			queue = append(queue, node{spec: reqPart, depth: cur.depth + 1})
		}
	}
	return allURLs, nil
}

func resolvePackageFiles(env platform.PrefetchExpandEnv, req *pypihandler.Requirement, pf config.PrefetchConfig) (files []string, metaURL string, err error) {
	pypi := env.Cfg.Platforms["pypi"]
	upstream := strings.TrimRight(strings.TrimSpace(pypi.Upstream), "/")
	if upstream == "" {
		return nil, "", fmt.Errorf("pypi 未配置上游")
	}
	indexURL := pypihandler.SimpleIndexURL(upstream, req.Name)
	body, _, err := env.Backend.FetchSimpleIndex(env.Ctx, indexURL, "pypi", env.Cfg.Cache.IndexTTLSeconds, true)
	if err != nil {
		return nil, "", err
	}
	refs := pypihandler.ExtractArtifactRefs(body, indexURL)
	hrefs := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.SHA256 != "" {
			env.Backend.RememberDigest(ref.URL, ref.SHA256)
		}
		hrefs = append(hrefs, ref.URL)
	}
	ver, matched, err := pypihandler.SelectFiles(hrefs, req)
	if err != nil {
		return nil, "", fmt.Errorf("%s（索引 %s）: %w", req.Raw, indexURL, err)
	}
	tags := mergeWheelTags(pf)
	plats := pf.TargetPlatformList()
	filtered := pypihandler.FilterArtifacts(matched, pf.ArtifactMode, tags, plats)
	if len(filtered) == 0 {
		if len(matched) == 0 {
			return nil, "", fmt.Errorf("未找到匹配文件")
		}
		return nil, "", fmt.Errorf(
			"未找到匹配文件（版本 %s 有 %d 个发行文件，均不符合预取策略 mode=%s python=%v tags=%v platforms=%v；可在「平台与上游」调整目标 Python/平台、补充额外 wheel 标签，或改用 artifact_mode=all）",
			ver, len(matched), pf.ArtifactMode, pf.TargetPythonVersions(), tags, plats,
		)
	}
	if env.Log != nil {
		env.Log.Info("prefetch selected version",
			zap.String("spec", req.Raw),
			zap.String("version", ver),
			zap.Int("files", len(filtered)),
			zap.String("mode", pf.ArtifactMode),
			zap.Strings("python", pf.TargetPythonVersions()),
			zap.Strings("tags", tags),
			zap.Strings("platforms", plats),
		)
	}
	metaURL = pickMetadataURL(filtered)
	return filtered, metaURL, nil
}

func mergeWheelTags(pf config.PrefetchConfig) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(t string) {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			return
		}
		if _, ok := seen[t]; ok {
			return
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	for _, t := range pypihandler.CPTagsFromVersions(pf.TargetPythonVersions()) {
		add(t)
	}
	for _, t := range pf.ExtraWheelTags {
		add(t)
	}
	return out
}

func pickMetadataURL(files []string) string {
	var wheel, anyFile string
	for _, u := range files {
		clean := stripURLNoise(u)
		fn := strings.ToLower(path.Base(clean))
		if strings.HasSuffix(fn, ".metadata") {
			return clean
		}
		if anyFile == "" {
			anyFile = clean
		}
		if strings.HasSuffix(fn, ".whl") {
			if strings.Contains(fn, "-none-any") {
				return clean + ".metadata"
			}
			if wheel == "" {
				wheel = clean
			}
		}
	}
	if wheel != "" {
		return wheel + ".metadata"
	}
	if anyFile != "" && strings.HasSuffix(strings.ToLower(path.Base(anyFile)), ".whl") {
		return anyFile + ".metadata"
	}
	return ""
}

func stripURLNoise(u string) string {
	clean := u
	if i := strings.IndexByte(clean, '#'); i >= 0 {
		clean = clean[:i]
	}
	if i := strings.IndexByte(clean, '?'); i >= 0 {
		clean = clean[:i]
	}
	return clean
}

func resolveMetadataUpstreamURL(metaOrFileURL string, pypiCfg config.PlatformConfig) string {
	raw := stripURLNoise(metaOrFileURL)
	if raw == "" {
		return ""
	}
	base := strings.TrimSpace(pypiCfg.MetadataUpstream)
	if base == "" {
		base = strings.TrimSpace(pypiCfg.FileUpstream)
	}
	if base == "" {
		return ""
	}
	base = strings.TrimRight(base, "/")

	pathPart := raw
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		pathPart = u.Path
	}
	idx := strings.Index(pathPart, "/packages/")
	if idx < 0 {
		if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
			return raw
		}
		return base + "/" + strings.TrimLeft(pathPart, "/")
	}
	pathPart = pathPart[idx:]
	if !strings.HasSuffix(strings.ToLower(pathPart), ".metadata") {
		pathPart += ".metadata"
	}
	return base + pathPart
}

func fetchRequires(env platform.PrefetchExpandEnv, metaURL string) ([]string, error) {
	pypiCfg := env.Cfg.Platforms["pypi"]
	target := resolveMetadataUpstreamURL(metaURL, pypiCfg)
	body, err := env.Backend.FetchMetadata(env.Ctx, target, "pypi", env.Cfg.Cache.IndexTTLSeconds, true)
	if err != nil {
		return nil, err
	}
	return pypihandler.ParseRequiresDist(body), nil
}
