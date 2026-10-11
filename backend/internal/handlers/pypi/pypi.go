package pypi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"strings"

	"github.com/livehl/mirrorhub/internal/config"
)

var hrefAttrRe = regexp.MustCompile(`(?i)(href=["'])([^"']+)(["'])`)

// wheelLinkRe 匹配包含 .whl 链接的 <a> 标签，用于注入 PEP 658 data-dist-info-metadata。
var wheelLinkRe = regexp.MustCompile(`(?i)(<a\s[^>]*href=["'][^"']+\.whl(?:#[^"']*)?["'][^>]*)(\s*>)`)

// requiresPythonAttrRe 用于把上游 HTML 实体（&gt;=）还原，避免 uv 解析 requires-python 失败。
var requiresPythonAttrRe = regexp.MustCompile(`(?i)(data-requires-python=["'])([^"']*)(["'])`)

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

// IndexETag 由原始索引正文 + 当前对外 base 生成，与改写结果语义一致。
func IndexETag(raw []byte, proxyBase string) string {
	h := sha256.New()
	_, _ = h.Write(raw)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(proxyBase))
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`
}

// RewriteURLs 逐链接解析改写上游 host，保留 digest fragment，并补全 HTML5 doctype。
func RewriteURLs(body []byte, contentType string, cfg config.Config) []byte {
	ct := strings.ToLower(contentType)
	if !strings.Contains(ct, "text/html") &&
		!strings.Contains(ct, "application/json") &&
		!strings.Contains(ct, "application/vnd.pypi.simple") {
		return body
	}
	hosts := rewriteHostSet(cfg)
	proxyBase := PublicBaseURL(cfg)
	prefixes := upstreamPathPrefixes(cfg)

	if strings.Contains(ct, "json") {
		out, err := rewriteJSONSimple(body, hosts, proxyBase, prefixes)
		if err == nil {
			return out
		}
		return body
	}

	out := hrefAttrRe.ReplaceAllFunc(body, func(m []byte) []byte {
		sub := hrefAttrRe.FindSubmatch(m)
		if len(sub) < 4 {
			return m
		}
		raw := string(sub[2])
		rewritten := rewriteOneURL(raw, hosts, proxyBase, prefixes)
		var b bytes.Buffer
		b.Write(sub[1])
		b.WriteString(rewritten)
		b.Write(sub[3])
		return b.Bytes()
	})
	// PEP 658: 为 .whl 链接注入 data-dist-info-metadata="true"，
	// 让 pip 只下载 metadata（几KB）而不是整个 wheel 来解析依赖。
	// 跳过上游已包含 data-dist-info-metadata 或 data-core-metadata 的标签。
	out = wheelLinkRe.ReplaceAllFunc(out, func(m []byte) []byte {
		lower := bytes.ToLower(m)
		if bytes.Contains(lower, []byte("data-dist-info-metadata")) ||
			bytes.Contains(lower, []byte("data-core-metadata")) {
			return m
		}
		sub := wheelLinkRe.FindSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		var b bytes.Buffer
		b.Write(sub[1])
		b.WriteString(` data-dist-info-metadata="true"`)
		b.Write(sub[2])
		return b.Bytes()
	})
	// 上游 simple 常把 >= 写成 &gt;=；uv 按字面解析会跳过全部版本。
	out = requiresPythonAttrRe.ReplaceAllFunc(out, func(m []byte) []byte {
		sub := requiresPythonAttrRe.FindSubmatch(m)
		if len(sub) < 4 {
			return m
		}
		raw := string(sub[2])
		unescaped := html.UnescapeString(raw)
		if unescaped == raw {
			return m
		}
		var b bytes.Buffer
		b.Write(sub[1])
		b.WriteString(unescaped)
		b.Write(sub[3])
		return b.Bytes()
	})
	return EnsureHTML5Document(out)
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
	// 官方 CDN 常见于镜像 HTML 绝对链接，始终纳入改写
	add("https://files.pythonhosted.org")
	add("https://pypi.org")
	if pypi, ok := cfg.Platforms["pypi"]; ok {
		add(pypi.Upstream)
		add(pypi.FileUpstream)
		add(pypi.MetadataUpstream)
	}
	return set
}

// upstreamPathPrefixes 收集上游 URL 的 path 前缀（如阿里云 /pypi），改写时剥掉以对齐本代理路由。
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
	if pypi, ok := cfg.Platforms["pypi"]; ok {
		add(pypi.Upstream)
		add(pypi.FileUpstream)
		add(pypi.MetadataUpstream)
	}
	// 长前缀优先，避免短前缀误伤
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if len(out[j]) > len(out[i]) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func stripUpstreamPathPrefix(path string, prefixes []string) string {
	for _, pref := range prefixes {
		if path == pref {
			return "/"
		}
		if strings.HasPrefix(path, pref+"/") {
			rest := path[len(pref):]
			// 仅当剥完后仍是本代理路由（/simple、/packages）才剥，避免误伤 Upstream=…/simple
			if strings.HasPrefix(rest, "/packages") || strings.HasPrefix(rest, "/simple") {
				return rest
			}
		}
	}
	return path
}

func rewriteOneURL(raw string, hosts map[string]struct{}, proxyBase string, prefixes []string) string {
	algo, hex, clean := ParseLinkDigest(raw)
	u, err := url.Parse(clean)
	if err != nil {
		return raw
	}
	if u.Host == "" {
		// 相对链接：仍剥上游 path 前缀（如 /pypi/packages → /packages）
		newPath := stripUpstreamPathPrefix(u.Path, prefixes)
		if newPath == u.Path {
			return raw
		}
		u.Path = newPath
		out := u.String()
		if algo != "" && hex != "" {
			out = out + "#" + algo + "=" + hex
		} else if i := strings.IndexByte(raw, '#'); i >= 0 {
			out = out + raw[i:]
		}
		return out
	}
	if _, ok := hosts[strings.ToLower(u.Host)]; !ok {
		return raw
	}
	base, err := url.Parse(proxyBase)
	if err != nil {
		return raw
	}
	u.Scheme = base.Scheme
	u.Host = base.Host
	u.Path = stripUpstreamPathPrefix(u.Path, prefixes)
	out := u.String()
	if algo != "" && hex != "" {
		out = out + "#" + algo + "=" + hex
	} else if i := strings.IndexByte(raw, '#'); i >= 0 {
		out = out + raw[i:]
	}
	return out
}

func rewriteJSONSimple(body []byte, hosts map[string]struct{}, proxyBase string, prefixes []string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	rewriteJSONValue(doc, hosts, proxyBase, prefixes)
	return json.Marshal(doc)
}

func rewriteJSONValue(v any, hosts map[string]struct{}, proxyBase string, prefixes []string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "url" {
				if s, ok := val.(string); ok {
					t[k] = rewriteOneURL(s, hosts, proxyBase, prefixes)
					continue
				}
			}
			rewriteJSONValue(val, hosts, proxyBase, prefixes)
		}
	case []any:
		for _, item := range t {
			rewriteJSONValue(item, hosts, proxyBase, prefixes)
		}
	}
}

// PrefersJSONAccept 判断客户端是否更想要 PEP 691 JSON Simple。
func PrefersJSONAccept(accept string) bool {
	a := strings.ToLower(accept)
	if a == "" || a == "*/*" {
		return false
	}
	if strings.Contains(a, "vnd.pypi.simple.v1+json") {
		return true
	}
	if strings.Contains(a, "application/json") && !strings.Contains(a, "text/html") {
		return true
	}
	return false
}

var (
	anchorTagRe = regexp.MustCompile(`(?is)<a\s+([^>]+)>(.*?)</a>`)
	attrRe      = regexp.MustCompile(`(?i)([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*=\s*("([^"]*)"|'([^']*)')`)
)

// HTMLSimpleToJSON 将 PEP 503 HTML 索引转为 PEP 691 JSON（镜像不支持 JSON Accept 时使用）。
func HTMLSimpleToJSON(body []byte, pageURL string) ([]byte, error) {
	name := packageFromSimpleURL(pageURL)
	base, _ := url.Parse(pageURL)
	files := make([]map[string]any, 0, 32)
	for _, m := range anchorTagRe.FindAllSubmatch(body, -1) {
		if len(m) < 3 {
			continue
		}
		attrs := parseHTMLAttrs(string(m[1]))
		href := strings.TrimSpace(attrs["href"])
		if href == "" || strings.HasPrefix(href, "#") {
			continue
		}
		abs := href
		if base != nil {
			if u, err := base.Parse(href); err == nil {
				abs = u.String()
			}
		}
		algo, hexDigest, clean := ParseLinkDigest(abs)
		filename := strings.TrimSpace(string(m[2]))
		filename = stripTags(filename)
		if filename == "" {
			if u, err := url.Parse(clean); err == nil {
				parts := strings.Split(strings.Trim(u.Path, "/"), "/")
				if len(parts) > 0 {
					filename = parts[len(parts)-1]
				}
			}
		}
		if filename == "" {
			continue
		}
		entry := map[string]any{
			"filename": filename,
			"url":      clean,
		}
		if algo == "sha256" && hexDigest != "" {
			entry["hashes"] = map[string]any{"sha256": hexDigest}
		}
		if rp := attrs["data-requires-python"]; rp != "" {
			entry["requires-python"] = rp
		}
		if attrs["data-yanked"] != "" {
			entry["yanked"] = true
		}
		files = append(files, entry)
	}
	doc := map[string]any{
		"meta":  map[string]any{"api-version": "1.0"},
		"name":  name,
		"files": files,
	}
	return json.Marshal(doc)
}

func parseHTMLAttrs(s string) map[string]string {
	out := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(s, -1) {
		if len(m) < 5 {
			continue
		}
		key := strings.ToLower(m[1])
		val := m[3]
		if val == "" {
			val = m[4]
		}
		out[key] = html.UnescapeString(val)
	}
	return out
}

var stripTagRe = regexp.MustCompile(`<[^>]+>`)

func stripTags(s string) string {
	return strings.TrimSpace(stripTagRe.ReplaceAllString(s, ""))
}

// EnsureHTML5Document 保证 simple 索引以 HTML5 doctype 开头，消除 pip 的 PEP 503 告警。
func EnsureHTML5Document(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return []byte("<!DOCTYPE html>\n<html><body></body></html>\n")
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return body
	}
	lower := bytes.ToLower(trimmed)
	if bytes.HasPrefix(lower, []byte("<!doctype html")) {
		// 上游可能有 \r\n 前缀，返回 trimmed 保证 doctype 在首字节
		if len(trimmed) == len(body) {
			return body
		}
		return trimmed
	}
	if bytes.HasPrefix(lower, []byte("<html")) {
		out := make([]byte, 0, len(body)+20)
		out = append(out, []byte("<!DOCTYPE html>\n")...)
		out = append(out, trimmed...)
		if trimmed[len(trimmed)-1] != '\n' {
			out = append(out, '\n')
		}
		return out
	}
	var b bytes.Buffer
	b.Grow(len(trimmed) + 64)
	b.WriteString("<!DOCTYPE html>\n<html><head><meta charset=\"utf-8\"></head><body>\n")
	b.Write(trimmed)
	b.WriteString("\n</body></html>\n")
	return b.Bytes()
}

