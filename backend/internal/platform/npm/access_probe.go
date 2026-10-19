package npm

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nihaoyanzu/cistern/internal/platform"
)

func (p *NPMPlatform) ProbeAccess(env platform.AccessProbeEnv) []platform.AccessProbeCheck {
	base := strings.TrimRight(strings.TrimSpace(env.Cfg.Upstream), "/")
	if base == "" {
		return []platform.AccessProbeCheck{{
			Name: "metadata", OK: false, MS: 0,
			Detail: "未配置上游",
		}}
	}

	// 用极小包 ms 验证：先拉 packument，再下载 tarball 前缀字节
	const pkg = "ms"
	metaURL := platform.JoinURL(base, "/"+pkg)
	t0 := time.Now()
	status, body, err := platform.ProbeGET(env.Ctx, env.Client, metaURL, 512<<10)
	ms := time.Since(t0).Milliseconds()

	var checks []platform.AccessProbeCheck
	if err != nil {
		return []platform.AccessProbeCheck{{
			Name: "metadata", OK: false, MS: ms,
			Detail: fmt.Sprintf("%s: %v", metaURL, err),
		}}
	}
	if status >= 400 {
		return []platform.AccessProbeCheck{{
			Name: "metadata", OK: false, MS: ms,
			Detail: fmt.Sprintf("%s → HTTP %d", metaURL, status),
		}}
	}
	checks = append(checks, platform.AccessProbeCheck{
		Name: "metadata", OK: true, MS: ms,
		Detail: fmt.Sprintf("%s → HTTP %d，%d 字节", metaURL, status, len(body)),
	})

	tarball := pickNPMTarball(body, base, pkg)
	if tarball == "" {
		checks = append(checks, platform.AccessProbeCheck{
			Name: "download", OK: false, MS: 0,
			Detail: "packument 中未找到 dist.tarball",
		})
		return checks
	}
	checks = append(checks, platform.ProbeDownloadSample(env.Ctx, env.Client, "download", tarball, nil))
	return checks
}

func pickNPMTarball(packument []byte, registryBase, pkg string) string {
	var doc struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Dist struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if json.Unmarshal(packument, &doc) != nil {
		return fallbackNPMTarball(registryBase, pkg)
	}
	ver := ""
	if doc.DistTags != nil {
		ver = strings.TrimSpace(doc.DistTags["latest"])
	}
	if ver != "" {
		if v, ok := doc.Versions[ver]; ok && strings.TrimSpace(v.Dist.Tarball) != "" {
			return v.Dist.Tarball
		}
	}
	for _, v := range doc.Versions {
		if u := strings.TrimSpace(v.Dist.Tarball); u != "" {
			return u
		}
	}
	return fallbackNPMTarball(registryBase, pkg)
}

func fallbackNPMTarball(registryBase, pkg string) string {
	// 经典 path：/{name}/-/{name}-{ver}.tgz
	return platform.JoinURL(registryBase, "/"+pkg+"/-/"+pkg+"-2.1.3.tgz")
}
