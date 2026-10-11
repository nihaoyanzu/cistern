package npm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/livehl/mirrorhub/internal/config"
)

// PublicBaseURL 对外访问根（请求期内由代理按访问 Host 填入 PublicHost）。
func PublicBaseURL(cfg config.Config) string {
	h := strings.TrimSpace(cfg.Server.PublicHost)
	h = strings.TrimRight(h, "/")
	if h == "" {
		return "http://127.0.0.1"
	}
	if strings.Contains(h, "://") {
		return h
	}
	return "http://" + h
}

// IndexETag 由 packument 正文 + 对外 base 生成。
func IndexETag(raw []byte, proxyBase string) string {
	h := sha256.New()
	_, _ = h.Write(raw)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(proxyBase))
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`
}

// RewritePackument 改写 packument / version 文档中的 dist.tarball 为本机下载口。
func RewritePackument(body []byte, cfg config.Config) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return body
	}
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}
	hosts := rewriteHostSet(cfg)
	proxyBase := PublicBaseURL(cfg)
	prefixes := upstreamPathPrefixes(cfg)
	rewriteValue(root, hosts, proxyBase, prefixes)
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

// ExtractTarballDigests 从 packument 提取 tarball URL 与 SRI integrity（多为 sha512-…）。
// 返回 map[tarballURL]integrity。
func ExtractTarballDigests(body []byte) map[string]string {
	out := map[string]string{}
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return out
	}
	collectDigests(root, out)
	return out
}

func collectDigests(v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		if dist, ok := x["dist"].(map[string]any); ok {
			tarball, _ := dist["tarball"].(string)
			integrity, _ := dist["integrity"].(string)
			if tarball != "" && integrity != "" {
				out[tarball] = integrity
			}
		}
		for _, child := range x {
			collectDigests(child, out)
		}
	case []any:
		for _, child := range x {
			collectDigests(child, out)
		}
	}
}

func rewriteValue(v any, hosts map[string]struct{}, proxyBase string, prefixes []string) {
	switch x := v.(type) {
	case map[string]any:
		if dist, ok := x["dist"].(map[string]any); ok {
			if tb, ok := dist["tarball"].(string); ok && tb != "" {
				dist["tarball"] = rewriteOneURL(tb, hosts, proxyBase, prefixes)
			}
		}
		for _, child := range x {
			rewriteValue(child, hosts, proxyBase, prefixes)
		}
	case []any:
		for _, child := range x {
			rewriteValue(child, hosts, proxyBase, prefixes)
		}
	}
}

func rewriteHostSet(cfg config.Config) map[string]struct{} {
	set := map[string]struct{}{}
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			set[strings.ToLower(u.Host)] = struct{}{}
			return
		}
		set[strings.ToLower(raw)] = struct{}{}
	}
	// 常见官方 / 国内镜像 CDN
	add("https://registry.npmjs.org")
	add("https://registry.npmjs.com")
	add("https://registry.npmmirror.com")
	add("https://cdn.npmmirror.com")
	add("https://r.cnpmjs.org")
	if npm, ok := cfg.Platforms["npm"]; ok {
		add(npm.Upstream)
		add(npm.FileUpstream)
	}
	return set
}

func upstreamPathPrefixes(cfg config.Config) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(raw string) {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u == nil {
			return
		}
		p := strings.TrimRight(u.Path, "/")
		if p == "" || p == "/" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if npm, ok := cfg.Platforms["npm"]; ok {
		add(npm.Upstream)
		add(npm.FileUpstream)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if len(out[j]) > len(out[i]) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func rewriteOneURL(raw string, hosts map[string]struct{}, proxyBase string, prefixes []string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return raw
	}
	host := strings.ToLower(u.Host)
	if _, ok := hosts[host]; !ok {
		return raw
	}
	path := u.EscapedPath()
	if path == "" {
		path = u.Path
	}
	for _, pre := range prefixes {
		if strings.HasPrefix(path, pre+"/") || path == pre {
			path = strings.TrimPrefix(path, pre)
			if path == "" {
				path = "/"
			}
			break
		}
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	out := strings.TrimRight(proxyBase, "/") + path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.Fragment
	}
	return out
}

// PrefersAbbreviated 判断客户端是否偏好 abbreviated metadata。
func PrefersAbbreviated(accept string) bool {
	return strings.Contains(strings.ToLower(accept), "vnd.npm.install-v1+json")
}

// IndexCacheVariant 元数据缓存键后缀：abbrev / full，避免双形态互相污染。
func IndexCacheVariant(accept string) string {
	if PrefersAbbreviated(accept) {
		return "abbrev"
	}
	return "full"
}

// DetectJSONContentType 为 npm 元数据选择合适的 Content-Type。
func DetectJSONContentType(contentType, accept string, body []byte) string {
	lowerCT := strings.ToLower(contentType)
	lowerAccept := strings.ToLower(accept)
	trimmed := bytes.TrimSpace(body)
	isJSON := strings.Contains(lowerCT, "json") ||
		(len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '['))
	if !isJSON {
		if contentType != "" {
			return contentType
		}
		return "application/octet-stream"
	}
	if strings.Contains(lowerCT, "vnd.npm.install-v1+json") ||
		strings.Contains(lowerAccept, "vnd.npm.install-v1+json") {
		return "application/vnd.npm.install-v1+json"
	}
	if strings.Contains(lowerCT, "vnd.npm") {
		return strings.TrimSpace(strings.Split(contentType, ";")[0])
	}
	return "application/json"
}
