package pypi

import (
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	pypihandler "github.com/nihaoyanzu/cistern/internal/handlers/pypi"
	"github.com/nihaoyanzu/cistern/internal/platform"
)

func (p *PyPIPlatform) ProbeAccess(env platform.AccessProbeEnv) []platform.AccessProbeCheck {
	upstream := strings.TrimRight(strings.TrimSpace(env.Cfg.Upstream), "/")
	fileUpstream := strings.TrimRight(strings.TrimSpace(env.Cfg.FileUpstream), "/")
	metaUpstream := env.Cfg.MetadataUpstream
	if upstream == "" {
		return []platform.AccessProbeCheck{{
			Name: "index_upstream", OK: false, MS: 0,
			Detail: "未配置上游",
		}}
	}

	var checks []platform.AccessProbeCheck
	var indexBody []byte
	indexURL := ""

	{
		indexURL = pypihandler.SimpleIndexURL(upstream, "pip")
		t0 := time.Now()
		status, bodyBytes, err := platform.ProbeGET(env.Ctx, env.Client, indexURL, 512<<10)
		ms := time.Since(t0).Milliseconds()
		if err != nil {
			checks = append(checks, platform.AccessProbeCheck{
				Name: "index_upstream", OK: false, MS: ms,
				Detail: fmt.Sprintf("%s: %v", indexURL, err),
			})
		} else if status >= 400 {
			checks = append(checks, platform.AccessProbeCheck{
				Name: "index_upstream", OK: false, MS: ms,
				Detail: fmt.Sprintf("%s → HTTP %d", indexURL, status),
			})
		} else {
			indexBody = bodyBytes
			refs := pypihandler.ExtractArtifactRefs(bodyBytes, indexURL)
			detail := fmt.Sprintf("%s → HTTP %d，解析到 %d 个发行文件", indexURL, status, len(refs))
			ok := len(refs) > 0 || strings.Contains(strings.ToLower(string(bodyBytes)), "pip")
			if !ok {
				detail += "（内容不像包索引）"
			}
			checks = append(checks, platform.AccessProbeCheck{
				Name: "index_upstream", OK: ok, MS: ms, Detail: detail,
			})
		}
	}

	{
		fileURL, how := pickFileProbeURL(indexBody, indexURL, fileUpstream)
		if fileURL == "" {
			checks = append(checks, platform.AccessProbeCheck{
				Name: "download", OK: false, MS: 0,
				Detail: "无法构造探测 URL（请检查索引上游是否可用）",
			})
		} else {
			c := platform.ProbeDownloadSample(env.Ctx, env.Client, "download", fileURL, nil)
			c.Detail = fmt.Sprintf("%s（%s）", c.Detail, how)
			checks = append(checks, c)
		}
	}

	{
		metaBase := strings.TrimSpace(metaUpstream)
		if metaBase == "" {
			checks = append(checks, platform.AccessProbeCheck{
				Name: "metadata_upstream", OK: true, Skipped: true, MS: 0,
				Detail: "未配置 metadata 上游",
			})
		} else {
			t0 := time.Now()
			metaURL := pickMetadataProbeURL(indexBody, indexURL, metaBase)
			if metaURL == "" {
				checks = append(checks, platform.AccessProbeCheck{
					Name: "metadata_upstream", OK: false, MS: 0,
					Detail: "无法从索引构造 .metadata 探测 URL",
				})
			} else {
				c := platform.ProbeDownloadSample(env.Ctx, env.Client, "metadata_upstream", metaURL, nil)
				if !c.OK && strings.Contains(c.Detail, "HTTP 404") {
					c.OK = true
					c.Detail = fmt.Sprintf("%s（该源可能不提供 PEP 658，预取依赖闭包会受限）", c.Detail)
					c.MS = time.Since(t0).Milliseconds()
				}
				checks = append(checks, c)
			}
		}
	}

	return checks
}

func pickFileProbeURL(indexBody []byte, indexURL, fileUpstream string) (string, string) {
	refs := pypihandler.ExtractArtifactRefs(indexBody, indexURL)
	for _, ref := range refs {
		u := strings.TrimSpace(ref.URL)
		if u == "" {
			continue
		}
		low := strings.ToLower(u)
		if strings.Contains(low, ".whl") || strings.HasSuffix(low, ".tar.gz") || strings.HasSuffix(low, ".zip") {
			return u, "索引链接"
		}
	}
	if len(refs) > 0 && strings.TrimSpace(refs[0].URL) != "" {
		return refs[0].URL, "索引链接"
	}
	base := strings.TrimRight(strings.TrimSpace(fileUpstream), "/")
	if base == "" {
		return "", ""
	}
	return base + "/", "文件上游根路径"
}

func pickMetadataProbeURL(indexBody []byte, indexURL, metaUpstream string) string {
	refs := pypihandler.ExtractArtifactRefs(indexBody, indexURL)
	var wheel string
	for _, ref := range refs {
		u := strings.TrimSpace(ref.URL)
		if strings.Contains(strings.ToLower(u), ".whl") {
			wheel = u
			break
		}
	}
	if wheel == "" {
		return ""
	}
	_, _, clean := pypihandler.ParseLinkDigest(wheel)
	wheel = clean

	metaBase := strings.TrimRight(strings.TrimSpace(metaUpstream), "/")
	if i := strings.Index(wheel, "/packages/"); i >= 0 {
		return metaBase + wheel[i:] + ".metadata"
	}
	if u, err := url.Parse(wheel); err == nil && u.Path != "" {
		p := u.Path
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		if strings.Contains(p, "/packages/") {
			return metaBase + p[strings.Index(p, "/packages/"):] + ".metadata"
		}
		return metaBase + path.Dir(p) + "/" + path.Base(p) + ".metadata"
	}
	return wheel + ".metadata"
}
