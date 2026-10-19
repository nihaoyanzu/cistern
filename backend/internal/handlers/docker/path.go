// Package docker 提供 Docker Registry V2 路径识别、匿名换票与镜像引用解析。
//
// 明确后置（不做）：用户登录、私有仓库凭证、push、多 registry 路由矩阵。
package docker

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/nihaoyanzu/cistern/internal/cachekey"
)

var (
	manifestPathRe = regexp.MustCompile(`(?i)^/v2/(.+)/manifests/([^/]+)$`)
	blobPathRe     = regexp.MustCompile(`(?i)^/v2/(.+)/blobs/(sha256:[a-f0-9]+)$`)
	imageRefRe     = regexp.MustCompile(`(?i)^(?:(?:[a-z0-9]+(?:[._-][a-z0-9]+)*)/)*[a-z0-9]+(?:[._-][a-z0-9]+)*(?:[:][a-z0-9][a-z0-9._-]*)?$`)
)

// IsV2Root Registry 探活路径。
func IsV2Root(path string) bool {
	path = normalizePath(path)
	return path == "/v2" || path == "/v2/"
}

// IsRegistryPath 是否为 /v2/... 业务路径（不含探活根）。
func IsRegistryPath(path string) bool {
	path = normalizePath(path)
	return strings.HasPrefix(path, "/v2/") && !IsV2Root(path)
}

// ParseManifestPath 解析 manifest 路径，返回 repo 与 reference（tag 或 digest）。
func ParseManifestPath(path string) (repo, reference string, ok bool) {
	m := manifestPathRe.FindStringSubmatch(normalizePath(path))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// ParseBlobPath 解析 blob 路径，返回 repo 与 digest。
func ParseBlobPath(path string) (repo, digest string, ok bool) {
	m := blobPathRe.FindStringSubmatch(normalizePath(path))
	if m == nil {
		return "", "", false
	}
	return m[1], strings.ToLower(m[2]), true
}

// IsBlobPath 是否为 layer/config blob。
func IsBlobPath(path string) bool {
	_, _, ok := ParseBlobPath(path)
	return ok
}

// IsManifestPath 是否为 manifest。
func IsManifestPath(path string) bool {
	_, _, ok := ParseManifestPath(path)
	return ok
}

// ManifestAcceptVariant 按 Accept 生成缓存键后缀，避免 list/image 互相污染。
func ManifestAcceptVariant(accept string) string {
	low := strings.ToLower(accept)
	switch {
	case strings.Contains(low, "manifest.list") || strings.Contains(low, "image.index"):
		return "list"
	case strings.Contains(low, "manifest.v2") || strings.Contains(low, "image.manifest"):
		return "v2"
	case strings.Contains(low, "manifest.v1"):
		return "v1"
	default:
		return "default"
	}
}

// DefaultManifestAccept 客户端未带 Accept 时的默认协商列表。
func DefaultManifestAccept() string {
	return strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.v1+json",
	}, ", ")
}

// ManifestCacheKey 索引缓存键（含 Accept 变体）。
func ManifestCacheKey(manifestURL, acceptVariant string) string {
	return "docker:index:" + cachekey.FromURL(manifestURL) + ":" + acceptVariant
}

// IsDockerBlobURL 是否为 registry blob 绝对 URL。
func IsDockerBlobURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Path == "" {
		return false
	}
	return IsBlobPath(u.Path)
}

// BlobCacheKey 按 digest 缓存，避免 CDN URL 抖动。
func BlobCacheKey(digest string) string {
	return "docker:blob:" + strings.ToLower(strings.TrimSpace(digest))
}

// NormalizeRepo 将单段官方镜像名补全为 library/xxx。
func NormalizeRepo(repo string) string {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo == "" {
		return repo
	}
	if !strings.Contains(repo, "/") {
		return "library/" + repo
	}
	return repo
}

// ImageRef 解析后的镜像引用。
type ImageRef struct {
	Repo string
	Tag  string // tag 或 digest（含 sha256: 前缀）
}

// ParseImageRef 解析 nginx:1.27 / library/redis:7 / alpine@sha256:...（不含 registry host）。
func ParseImageRef(raw string) (ImageRef, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return ImageRef{}, false
	}
	// 去掉偶发的 docker.io/ 前缀
	raw = strings.TrimPrefix(raw, "docker.io/")
	raw = strings.TrimPrefix(raw, "index.docker.io/")
	if strings.Contains(raw, "://") {
		return ImageRef{}, false
	}
	// host:port/ 形式暂不支持（多 registry 后置）
	if i := strings.Index(raw, "/"); i > 0 {
		hostPart := raw[:i]
		if strings.Contains(hostPart, ".") || strings.Contains(hostPart, ":") {
			return ImageRef{}, false
		}
	}

	var repo, tag string
	if strings.Contains(raw, "@") {
		parts := strings.SplitN(raw, "@", 2)
		repo, tag = parts[0], parts[1]
	} else if i := strings.LastIndex(raw, ":"); i > 0 && !strings.Contains(raw[i+1:], "/") {
		repo, tag = raw[:i], raw[i+1:]
	} else {
		// 禁止裸名默认 :latest，否则 cryptography / django 等 PyPI 包会被误判为 Docker 镜像
		return ImageRef{}, false
	}
	repo = NormalizeRepo(repo)
	if repo == "" || tag == "" {
		return ImageRef{}, false
	}
	if !imageRefRe.MatchString(strings.TrimPrefix(repo, "library/")+":"+strings.TrimPrefix(tag, "sha256:")) &&
		!strings.HasPrefix(tag, "sha256:") {
		// 宽松：允许 library/ 与 digest
		if !strings.Contains(repo, "/") {
			return ImageRef{}, false
		}
	}
	return ImageRef{Repo: repo, Tag: tag}, true
}

// LookLikeImageList 粗判粘贴内容是否为 docker image 列表（而非 requirements/lockfile）。
// 要求行内含 : 或 @，避免把裸包名（如 django）误判为 library/django:latest。
func LookLikeImageList(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "{") || strings.Contains(strings.ToLower(s[:min(len(s), 200)]), "lockfileversion") {
		return false
	}
	lines := 0
	hits := 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines++
		if !strings.Contains(line, ":") && !strings.Contains(line, "@") {
			continue
		}
		// 排除 PEP 508 环境标记等含冒号但不像镜像的行
		if strings.Contains(line, ";") || strings.Contains(line, " ") {
			continue
		}
		if _, ok := ParseImageRef(line); ok {
			hits++
		}
	}
	return lines > 0 && hits*2 >= lines // 至少一半行像镜像引用
}

// ParseImageList 解析多行 image:tag，返回引用列表与跳过行。
func ParseImageList(text string) (refs []ImageRef, skipped []string) {
	seen := map[string]struct{}{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ref, ok := ParseImageRef(line)
		if !ok {
			skipped = append(skipped, line)
			continue
		}
		key := ref.Repo + "@" + ref.Tag
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		refs = append(refs, ref)
	}
	return refs, skipped
}

// RegistryBlobURL 构造上游 blob URL。
func RegistryBlobURL(registryBase, repo, digest string) string {
	base := strings.TrimRight(strings.TrimSpace(registryBase), "/")
	repo = strings.Trim(repo, "/")
	digest = strings.ToLower(strings.TrimSpace(digest))
	return base + "/v2/" + repo + "/blobs/" + digest
}

// RegistryManifestURL 构造上游 manifest URL。
func RegistryManifestURL(registryBase, repo, reference string) string {
	base := strings.TrimRight(strings.TrimSpace(registryBase), "/")
	repo = strings.Trim(repo, "/")
	reference = strings.TrimSpace(reference)
	// tag 需 path-escape；digest 含冒号
	esc := url.PathEscape(reference)
	if strings.HasPrefix(reference, "sha256:") {
		esc = reference // digest 保持原样
	}
	return base + "/v2/" + repo + "/manifests/" + esc
}

func normalizePath(path string) string {
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if len(path) > 1 && strings.HasSuffix(path, "/") && path != "/v2/" {
		// 保留 /v2/；其它尾斜杠去掉便于正则
		if !strings.HasPrefix(path, "/v2/") {
			path = strings.TrimRight(path, "/")
		}
	}
	return path
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
