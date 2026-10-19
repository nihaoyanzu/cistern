// Package pypi 实现 PyPI 平台的 Platform 接口。
package pypi

import (
	"bytes"
	"strings"

	"github.com/nihaoyanzu/cistern/internal/config"
	pypihandler "github.com/nihaoyanzu/cistern/internal/handlers/pypi"
	"github.com/nihaoyanzu/cistern/internal/platform"
	"github.com/nihaoyanzu/cistern/internal/router"
)

func init() {
	platform.Register(&PyPIPlatform{})
}

// PyPIPlatform 实现 platform.Platform 接口。
type PyPIPlatform struct{}

func (p *PyPIPlatform) Name() string { return "pypi" }

func (p *PyPIPlatform) Route(path string, cfg config.PlatformConfig) *router.Match {
	return MatchPyPI(path, Routes{
		Upstream:         cfg.Upstream,
		FileUpstream:     cfg.FileUpstream,
		MetadataUpstream: cfg.MetadataUpstream,
	})
}

type Routes struct {
	Upstream         string
	FileUpstream     string
	MetadataUpstream string
}

// MatchPyPI 将本代理路径映射到上游 TargetURL。
func MatchPyPI(path string, r Routes) *router.Match {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	up := strings.TrimRight(strings.TrimSpace(r.Upstream), "/")
	fileUp := strings.TrimRight(strings.TrimSpace(r.FileUpstream), "/")
	if up == "" || fileUp == "" {
		return nil
	}
	switch {
	case strings.HasPrefix(path, "/simple"):
		return &router.Match{
			Platform:       "pypi",
			Strategy:       router.StrategyProxy,
			UpstreamBase:   up,
			TargetURL:      up + path,
			IsIndex:        true,
			SmallFileBoost: true,
		}
	case strings.HasPrefix(path, "/packages") && strings.HasSuffix(path, ".metadata"):
		if !pypihandler.IsPackageArtifactPath(path) {
			return nil
		}
		metaUpstream := strings.TrimRight(strings.TrimSpace(r.MetadataUpstream), "/")
		if metaUpstream == "" {
			return nil
		}
		return &router.Match{
			Platform:     "pypi",
			Strategy:     router.StrategyProxy,
			UpstreamBase: metaUpstream,
			TargetURL:    metaUpstream + path,
			IsMetadata:   true,
		}
	case strings.HasPrefix(path, "/packages"):
		// 仅发行文件；裸 /packages 或目录页会命中上游 HTML 浏览页，一律不代理
		if !pypihandler.IsPackageArtifactPath(path) {
			return nil
		}
		return &router.Match{
			Platform:     "pypi",
			Strategy:     router.StrategyParallel,
			UpstreamBase: fileUp,
			TargetURL:    fileUp + path,
		}
	default:
		return nil
	}
}

func (p *PyPIPlatform) TransformIndex(body []byte, contentType string, cfg config.Config, accept, pageURL string) platform.IndexResult {
	ct := platform.DetectContentType(contentType, body)
	body, ct = negotiateSimpleIndex(body, ct, accept, pageURL)
	proxyBase := pypihandler.PublicBaseURL(cfg)
	out := pypihandler.RewriteURLs(body, ct, cfg)
	etag := pypihandler.IndexETag(out, proxyBase)
	return platform.IndexResult{
		Body:        out,
		ETag:        etag,
		ContentType: ct,
	}
}

func negotiateSimpleIndex(body []byte, contentType, accept, pageURL string) ([]byte, string) {
	if !pypihandler.PrefersJSONAccept(accept) {
		return body, contentType
	}
	lower := strings.ToLower(contentType)
	trimmed := bytes.TrimSpace(body)
	if strings.Contains(lower, "json") || (len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')) {
		return body, "application/vnd.pypi.simple.v1+json"
	}
	out, err := pypihandler.HTMLSimpleToJSON(body, pageURL)
	if err != nil || len(out) == 0 {
		return body, contentType
	}
	return out, "application/vnd.pypi.simple.v1+json"
}
