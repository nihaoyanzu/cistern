// Package huggingface 实现 Hugging Face Hub 只读缓存平台。
//
// 范围：公开 models/datasets 的 API 与 resolve/raw；可选服务端 UpstreamToken（gated）。
// 明确后置：终端用户登录透传、Spaces、上传、完整 Xet CAS/CDN 代理、git clone 协议。
package huggingface

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/nihaoyanzu/cistern/internal/config"
	hfhandler "github.com/nihaoyanzu/cistern/internal/handlers/huggingface"
	"github.com/nihaoyanzu/cistern/internal/platform"
	"github.com/nihaoyanzu/cistern/internal/router"
)

func init() {
	platform.Register(&HuggingFacePlatform{})
}

// HuggingFacePlatform 实现 platform.Platform。
type HuggingFacePlatform struct{}

func (p *HuggingFacePlatform) Name() string { return "huggingface" }

func (p *HuggingFacePlatform) Route(path string, cfg config.PlatformConfig) *router.Match {
	return MatchHuggingFace(path, Routes{
		Upstream:     cfg.Upstream,
		FileUpstream: cfg.FileUpstream,
	})
}

// Routes 上游配置。
type Routes struct {
	Upstream     string
	FileUpstream string
}

// MatchHuggingFace 映射 Hub API / resolve / raw。
func MatchHuggingFace(path string, r Routes) *router.Match {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if !hfhandler.IsHuggingFacePath(path) {
		return nil
	}

	apiUp := strings.TrimRight(strings.TrimSpace(r.Upstream), "/")
	fileUp := strings.TrimRight(strings.TrimSpace(r.FileUpstream), "/")
	if apiUp == "" || fileUp == "" {
		return nil
	}

	if hfhandler.IsPackagePath(path) {
		return &router.Match{
			Platform:     "huggingface",
			Strategy:     router.StrategyParallel,
			UpstreamBase: fileUp,
			TargetURL:    fileUp + path,
			ReadOnly:     true,
		}
	}

	return &router.Match{
		Platform:       "huggingface",
		Strategy:       router.StrategyProxy,
		UpstreamBase:   apiUp,
		TargetURL:      apiUp + path,
		IsIndex:        true,
		SmallFileBoost: true,
		ReadOnly:       true,
	}
}

func (p *HuggingFacePlatform) TransformIndex(body []byte, contentType string, cfg config.Config, accept, pageURL string) platform.IndexResult {
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
	ct := hfhandler.DetectContentType(path, contentType, body)
	sum := sha256.Sum256(body)
	return platform.IndexResult{
		Body:        body,
		ETag:        `"` + hex.EncodeToString(sum[:]) + `"`,
		ContentType: ct,
	}
}
