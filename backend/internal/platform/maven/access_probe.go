package maven

import (
	"strings"

	"github.com/nihaoyanzu/cistern/internal/platform"
)

func (p *MavenPlatform) ProbeAccess(env platform.AccessProbeEnv) []platform.AccessProbeCheck {
	base := strings.TrimRight(strings.TrimSpace(env.Cfg.Upstream), "/")
	if base == "" {
		return []platform.AccessProbeCheck{{
			Name: "download", OK: false, MS: 0,
			Detail: "未配置上游",
		}}
	}
	// 下载极小 POM，验证仓库路径与制品拉取（仓库根常 404，不能当通过依据）
	pom := platform.JoinURL(base, "/junit/junit/4.13.2/junit-4.13.2.pom")
	return []platform.AccessProbeCheck{
		platform.ProbeDownloadSample(env.Ctx, env.Client, "download", pom, nil),
	}
}
