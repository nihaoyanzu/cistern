package goproxy

import (
	"strings"

	"github.com/nihaoyanzu/cistern/internal/platform"
)

func (p *GoproxyPlatform) ProbeAccess(env platform.AccessProbeEnv) []platform.AccessProbeCheck {
	base := strings.TrimRight(strings.TrimSpace(env.Cfg.Upstream), "/")
	if base == "" {
		return []platform.AccessProbeCheck{{
			Name: "download", OK: false, MS: 0,
			Detail: "未配置上游",
		}}
	}
	// 下载小模块 zip 前缀字节，验证模块拉取通路
	zipURL := platform.JoinURL(base, "/rsc.io/quote/@v/v1.5.2.zip")
	return []platform.AccessProbeCheck{
		platform.ProbeDownloadSample(env.Ctx, env.Client, "download", zipURL, nil),
	}
}
