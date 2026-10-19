package huggingface

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"go.uber.org/zap"

	"github.com/nihaoyanzu/cistern/internal/cache"
	hfhandler "github.com/nihaoyanzu/cistern/internal/handlers/huggingface"
	"github.com/nihaoyanzu/cistern/internal/platform"
)

// HF 优先于 Docker：无 tag 的 owner/repo 只归 HF（Docker 不认裸名）。
// 与 Go module / digest 的形态冲突由 ParseRepoRef 正规则消化，不依赖其它平台。
const prefetchPriorityHF = 100

func (p *HuggingFacePlatform) PrefetchPriority() int { return prefetchPriorityHF }

func (p *HuggingFacePlatform) OwnsPrefetchItem(item string) bool {
	item = strings.TrimSpace(item)
	if item == "" {
		return false
	}
	if hfhandler.IsHuggingFaceArtifactURL(item) {
		return true
	}
	if strings.HasPrefix(item, "http://") || strings.HasPrefix(item, "https://") {
		if u, err := url.Parse(item); err == nil && u.Path != "" {
			if hfhandler.IsPackagePath(u.Path) || hfhandler.IsAPIPath(u.Path) {
				return true
			}
		}
		_, ok := hfhandler.ParseRepoRef(item)
		return ok
	}
	_, ok := hfhandler.ParseRepoRef(item)
	return ok
}

func (p *HuggingFacePlatform) ExpandPrefetchItem(env platform.PrefetchExpandEnv, item string) ([]string, error) {
	if strings.HasPrefix(item, "http://") || strings.HasPrefix(item, "https://") {
		if u, err := url.Parse(item); err == nil && u.Path != "" {
			if hfhandler.IsPackagePath(u.Path) || hfhandler.IsAPIPath(u.Path) {
				return []string{item}, nil
			}
		}
		if hfhandler.IsHuggingFaceArtifactURL(item) {
			return []string{item}, nil
		}
	}
	ref, ok := hfhandler.ParseRepoRef(item)
	if !ok {
		return nil, fmt.Errorf("无效 huggingface 仓库引用: %s", item)
	}
	pcfg, ok := env.Cfg.Platforms["huggingface"]
	if !ok || !pcfg.Enabled {
		return nil, fmt.Errorf("huggingface 模块未启用")
	}
	up := strings.TrimRight(strings.TrimSpace(pcfg.Upstream), "/")
	if up == "" {
		return nil, fmt.Errorf("huggingface 未配置上游")
	}
	fileUp := strings.TrimRight(strings.TrimSpace(pcfg.FileUpstream), "/")
	if fileUp == "" {
		return nil, fmt.Errorf("huggingface 未配置文件上游")
	}
	treeURL := hfhandler.TreeAPIURL(up, ref.RepoType, ref.ID, ref.Revision)
	headers := http.Header{}
	_ = p.InjectUpstreamAuth(env.Ctx, headers, env.Cfg, "")
	status, respHeader, body, getErr := env.Backend.ProxyBytes(env.Ctx, http.MethodGet, treeURL, headers, nil, "huggingface", true)
	if getErr != nil || status < 200 || status >= 300 {
		return nil, fmt.Errorf("hf tree %s: status=%d err=%v", treeURL, status, getErr)
	}
	ct := hfhandler.DetectContentType("/api/", respHeader.Get("Content-Type"), body)
	key := "huggingface:index:" + cache.KeyFromURL(treeURL)
	if _, err := env.Backend.PutBytes(key, body, ct, env.Cfg.Cache.IndexTTLSeconds, cache.Meta{
		SourceURL:    treeURL,
		Kind:         "index",
		UpstreamETag: respHeader.Get("ETag"),
	}); err != nil && env.Log != nil {
		env.Log.Warn("hf tree cache put failed", zap.String("key", key), zap.Error(err))
	}
	entries, err := hfhandler.ParseTreeEntries(body)
	if err != nil {
		return nil, fmt.Errorf("hf tree parse: %w", err)
	}
	files := hfhandler.FilePathsFromTree(entries)
	urls := make([]string, 0, len(files))
	for _, fp := range files {
		urls = append(urls, hfhandler.ResolveFileURL(fileUp, ref.RepoType, ref.ID, ref.Revision, fp))
	}
	if env.Log != nil {
		env.Log.Info("prefetch huggingface resolved",
			zap.String("repo", ref.ID),
			zap.String("type", ref.RepoType),
			zap.String("revision", ref.Revision),
			zap.Int("files", len(urls)),
		)
	}
	return urls, nil
}
