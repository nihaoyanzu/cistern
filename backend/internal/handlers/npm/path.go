// Package npm 提供 npm registry 路径识别、lockfile / package.json 解析。
package npm

import (
	"net/url"
	"strings"
)

// IsTarballPath 判断是否为 registry 制品路径（含 scoped）。
func IsTarballPath(path string) bool {
	path = normalizePath(path)
	if !strings.Contains(path, "/-/") {
		return false
	}
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tar.gz")
}

// IsReservedProxyPath 非 npm 业务路径，避免抢走 PyPI / 健康检查等。
func IsReservedProxyPath(path string) bool {
	path = normalizePath(path)
	switch {
	case path == "/" || path == "/health":
		return true
	case strings.HasPrefix(path, "/simple"), strings.HasPrefix(path, "/packages"):
		return true
	case strings.HasPrefix(path, "/api/"):
		return true
	case strings.HasPrefix(path, "/v2/") || path == "/v2":
		return true
	case strings.Contains(path, "/@v/") || strings.HasSuffix(path, "/@latest") || strings.HasPrefix(path, "/sumdb/"):
		// Go module proxy / sumdb，避免 npm 抢路由
		return true
	case strings.Contains(path, "/resolve/") || strings.Contains(path, "/raw/") ||
		strings.HasPrefix(path, "/datasets/") ||
		strings.HasPrefix(path, "/api/models") || strings.HasPrefix(path, "/api/datasets") ||
		strings.HasPrefix(path, "/api/resolve-cache/"):
		// Hugging Face Hub，避免 npm 抢路由
		return true
	case strings.Contains(path, "/maven-metadata.xml") ||
		strings.HasPrefix(path, "/maven2/") || path == "/maven2":
		// Maven2 布局，避免 npm 抢路由
		return true
	default:
		// 双保险：形如 …/artifact/version/artifact-version.* 的 Maven 制品路径
		if looksLikeMavenArtifactPath(path) {
			return true
		}
		return false
	}
}

func looksLikeMavenArtifactPath(path string) bool {
	path = normalizePath(path)
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 4 {
		return false
	}
	file := parts[len(parts)-1]
	artifact := parts[len(parts)-3]
	lower := strings.ToLower(file)
	if !strings.HasPrefix(file, artifact+"-") {
		return false
	}
	for _, ext := range []string{".pom", ".jar", ".aar", ".war", ".module", ".zip",
		".pom.sha1", ".jar.sha1", ".pom.md5", ".jar.md5",
		".pom.sha256", ".jar.sha256", ".pom.sha512", ".jar.sha512"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// IsTarballURL 判断绝对 URL 是否像 npm tarball（预取选平台用）。
func IsTarballURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Path == "" {
		return false
	}
	return IsTarballPath(u.Path)
}

func normalizePath(path string) string {
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}
