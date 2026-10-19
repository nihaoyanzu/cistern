// Package platid 按缓存 key / SourceURL 判定所属平台，与包列表、Stats.by_platform 同源。
package platid

import (
	"net/url"
	"strings"

	dockerhandler "github.com/nihaoyanzu/cistern/internal/handlers/docker"
	goproxyhandler "github.com/nihaoyanzu/cistern/internal/handlers/goproxy"
	hfhandler "github.com/nihaoyanzu/cistern/internal/handlers/huggingface"
	mavenhandler "github.com/nihaoyanzu/cistern/internal/handlers/maven"
	npmhandler "github.com/nihaoyanzu/cistern/internal/handlers/npm"
	pypihandler "github.com/nihaoyanzu/cistern/internal/handlers/pypi"
)

// Order 判定顺序：更具体的平台优先。
var Order = []string{"docker", "huggingface", "goproxy", "maven", "npm", "pypi"}

// Of 返回条目所属平台；无法归类为 other。
func Of(key, sourceURL, kind string) string {
	for _, p := range Order {
		if Belongs(p, key, sourceURL, kind) {
			return p
		}
	}
	return "other"
}

// Belongs 判断条目是否属于指定平台。
func Belongs(platform, key, sourceURL, kind string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	src := strings.TrimSpace(sourceURL)
	kind = strings.TrimSpace(kind)
	switch platform {
	case "pypi":
		// 索引键为 pypi:index:…；根 /simple/ 无包名，不能仅靠 ParseArtifactURL.Name
		if strings.HasPrefix(key, "pypi:") {
			return true
		}
		if src == "" {
			return false
		}
		if kind == "index" || strings.Contains(src, "/simple/") {
			return true
		}
		if kind == "metadata" || kind == "package" {
			info := pypihandler.ParseArtifactURL(src, kind)
			return info.Name != "" || strings.Contains(src, "/packages/")
		}
		info := pypihandler.ParseArtifactURL(src, kind)
		return info.Name != ""
	case "huggingface":
		if strings.HasPrefix(key, "huggingface:") || strings.HasPrefix(key, "hf:") {
			return true
		}
		if u, err := url.Parse(src); err == nil && u.Path != "" {
			return hfhandler.IsHuggingFacePath(u.Path)
		}
		return false
	case "goproxy":
		if strings.HasPrefix(key, "goproxy:") {
			return true
		}
		if u, err := url.Parse(src); err == nil && u.Path != "" {
			return goproxyhandler.IsGoproxyPath(u.Path)
		}
		return false
	case "docker":
		if strings.HasPrefix(key, "docker:") {
			return true
		}
		return dockerhandler.IsDockerBlobURL(src) || strings.Contains(src, "/v2/")
	case "maven":
		if strings.HasPrefix(key, "maven:") {
			return true
		}
		return mavenhandler.IsMavenArtifactURL(src)
	case "npm":
		if strings.HasPrefix(key, "npm:") {
			return true
		}
		return npmhandler.IsTarballURL(src) || looksLikeNPMPackument(src)
	default:
		return false
	}
}

func looksLikeNPMPackument(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Path == "" {
		return false
	}
	p := u.Path
	if npmhandler.IsTarballPath(p) {
		return true
	}
	if strings.HasPrefix(p, "/@") {
		return strings.Count(p, "/") >= 2 && !strings.Contains(p, "/-/")
	}
	// 单段路径易误伤 PyPI 根 /simple/、Docker /v2 等
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if len(segs) != 1 || segs[0] == "" || strings.Contains(segs[0], ".") {
		return false
	}
	switch strings.ToLower(segs[0]) {
	case "simple", "packages", "v2", "maven2", "repository", "sumdb":
		return false
	}
	return true
}
