package huggingface

import (
	"context"
	"net/http"
	"strings"

	"github.com/livehl/mirrorhub/internal/config"
	hfhandler "github.com/livehl/mirrorhub/internal/handlers/huggingface"
)

func (p *HuggingFacePlatform) InjectUpstreamAuth(_ context.Context, headers http.Header, cfg config.Config, _ string) error {
	pcfg := cfg.Platforms["huggingface"]
	tok := strings.TrimSpace(pcfg.UpstreamToken)
	if tok == "" {
		return nil
	}
	headers.Set("Authorization", "Bearer "+tok)
	return nil
}

func (p *HuggingFacePlatform) PackageCacheIdentity(_, targetURL string, reqHeader http.Header, defaultKey string) (string, string) {
	// 固定 hf:pkg: + KeyFromURL(origURL)。勿用请求头 X-Linked-Etag 换键：
	// huggingface_hub 交互下载常带 Linked-Etag，旧逻辑会落到 hf:blob:…；
	// 预取展开无此头仍用 hf:pkg:…，大文件交互下完后预取会整包重下，且两套键无法互相命中。
	_ = targetURL
	_ = reqHeader
	return "hf:pkg:" + defaultKey, ""
}

func (p *HuggingFacePlatform) DetectIndexContentType(upstreamCT, _ string, body []byte) string {
	return hfhandler.DetectContentType("", upstreamCT, body)
}
