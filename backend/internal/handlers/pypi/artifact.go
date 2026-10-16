package pypi

import (
	"net/url"
	"path"
	"strings"
)

// ArtifactInfo 从发行文件名 / URL 解析出的包信息
type ArtifactInfo struct {
	Filename string `json:"filename"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Type     string `json:"type"` // wheel | sdist | index | other
}

// ParseArtifactURL 从源 URL 推断包名/版本/文件名
func ParseArtifactURL(raw, kind string) ArtifactInfo {
	info := ArtifactInfo{Type: "other"}
	if kind == "index" || strings.Contains(raw, "/simple/") {
		info.Type = "index"
		info.Name = packageFromSimpleURL(raw)
		info.Filename = ""
		return info
	}
	u, err := url.Parse(raw)
	fn := ""
	if err == nil {
		fn = path.Base(u.Path)
	} else {
		fn = path.Base(raw)
	}
	info = ParseArtifactFilename(fn)
	if info.Name == "" {
		info.Name = packageFromPackagesPath(raw)
	}
	return info
}

// ParseArtifactFilename 解析 wheel/sdist 文件名（含 .whl.metadata / .tar.gz.metadata）。
func ParseArtifactFilename(filename string) ArtifactInfo {
	base := path.Base(filename)
	info := ArtifactInfo{Filename: base, Type: "other"}
	parseAs := base
	isMeta := false
	if len(parseAs) > 9 && strings.EqualFold(parseAs[len(parseAs)-9:], ".metadata") {
		parseAs = parseAs[:len(parseAs)-9]
		isMeta = true
	}
	if m := wheelTag.FindStringSubmatch(parseAs); len(m) == 5 {
		info.Name = NormalizeName(m[1])
		info.Version = m[2]
		if isMeta {
			info.Type = "metadata"
		} else {
			info.Type = "wheel"
		}
		return info
	}
	if m := sdistTag.FindStringSubmatch(parseAs); len(m) == 4 {
		info.Name = NormalizeName(m[1])
		info.Version = m[2]
		if isMeta {
			info.Type = "metadata"
		} else {
			info.Type = "sdist"
		}
		return info
	}
	return info
}

func packageFromSimpleURL(raw string) string {
	u, err := url.Parse(raw)
	p := raw
	if err == nil {
		p = u.Path
	}
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i := 0; i < len(parts); i++ {
		if parts[i] == "simple" && i+1 < len(parts) {
			return NormalizeName(parts[i+1])
		}
	}
	return ""
}

func packageFromPackagesPath(raw string) string {
	u, err := url.Parse(raw)
	p := raw
	if err == nil {
		p = u.Path
	}
	// 旧布局 /packages/.../name/file；新布局 /packages/ab/cd/<sha256>/file（倒数第二段是哈希，不可当包名）
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i, seg := range parts {
		if seg == "packages" && i+3 < len(parts) {
			parent := parts[len(parts)-2]
			if looksLikeContentHash(parent) {
				return ""
			}
			return NormalizeName(parent)
		}
	}
	return ""
}

// looksLikeContentHash 判断是否为 PyPI 内容寻址目录（长十六进制）。
func looksLikeContentHash(s string) bool {
	if len(s) < 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// IsPackageArtifactPath 判断是否为发行文件路径（拒绝镜像站 HTML 目录页）。
func IsPackageArtifactPath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	base := path.Base(p)
	if base == "" || base == "." || base == "/" || base == "packages" {
		return false
	}
	low := strings.ToLower(base)
	switch {
	case strings.HasSuffix(low, ".whl"),
		strings.HasSuffix(low, ".zip"),
		strings.HasSuffix(low, ".egg"),
		strings.HasSuffix(low, ".exe"),
		strings.HasSuffix(low, ".tar.gz"),
		strings.HasSuffix(low, ".tar.bz2"),
		strings.HasSuffix(low, ".tar.xz"),
		strings.HasSuffix(low, ".tgz"),
		strings.HasSuffix(low, ".metadata"):
		return true
	default:
		return false
	}
}

// IsHTMLContentType 判断上游是否返回了网页（目录浏览页等）。
func IsHTMLContentType(ct string) bool {
	return strings.Contains(strings.ToLower(ct), "text/html")
}
