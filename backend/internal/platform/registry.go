package platform

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/nihaoyanzu/cistern/internal/config"
	"github.com/nihaoyanzu/cistern/internal/router"
)

var (
	mu        sync.RWMutex
	platforms []Platform
)

// Register 注册一个平台实现，应在 init() 中调用。
func Register(p Platform) {
	mu.Lock()
	defer mu.Unlock()
	platforms = append(platforms, p)
}

// All 返回所有已注册平台。
func All() []Platform {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Platform, len(platforms))
	copy(out, platforms)
	return out
}

// ByName 按平台名查找已注册实例；未找到返回 nil。
func ByName(name string) Platform {
	mu.RLock()
	defer mu.RUnlock()
	for _, p := range platforms {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// PrefetchExpanders 按 PrefetchPriority 降序返回实现了 PrefetchExpander 的平台。
func PrefetchExpanders() []PrefetchExpander {
	mu.RLock()
	defer mu.RUnlock()
	type pair struct {
		p PrefetchExpander
		n int
	}
	var list []pair
	for _, p := range platforms {
		if e, ok := p.(PrefetchExpander); ok {
			list = append(list, pair{e, e.PrefetchPriority()})
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].n > list[j].n })
	out := make([]PrefetchExpander, len(list))
	for i, x := range list {
		out[i] = x.p
	}
	return out
}

// LookupPrefetchOwner 返回 OwnsPrefetchItem 为真的最高优先级平台。
func LookupPrefetchOwner(item string) Platform {
	for _, e := range PrefetchExpanders() {
		if e.OwnsPrefetchItem(item) {
			if p, ok := e.(Platform); ok {
				return p
			}
		}
	}
	return nil
}

// TextDetectors 按 TextDetectPriority 降序。
func TextDetectors() []TextDetector {
	mu.RLock()
	defer mu.RUnlock()
	type pair struct {
		d TextDetector
		n int
	}
	var list []pair
	for _, p := range platforms {
		if d, ok := p.(TextDetector); ok {
			list = append(list, pair{d, d.TextDetectPriority()})
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].n > list[j].n })
	out := make([]TextDetector, len(list))
	for i, x := range list {
		out[i] = x.d
	}
	return out
}

// MatchResult 包含路由匹配结果和对应的平台实例。
type MatchResult struct {
	Platform Platform
	Match    *router.Match
}

// Match 按注册顺序尝试匹配请求路径，返回第一个匹配的平台。
func Match(path string, cfg config.Config) *MatchResult {
	mu.RLock()
	defer mu.RUnlock()
	for _, p := range platforms {
		pcfg, ok := cfg.Platforms[p.Name()]
		if !ok || !pcfg.Enabled {
			continue
		}
		if m := p.Route(path, pcfg); m != nil {
			return &MatchResult{Platform: p, Match: m}
		}
	}
	return nil
}

// ── 通用 HTTP 工具函数 ────────────────────────────────────────────

// FilterRequestHeaders 过滤请求头，去除逐跳头。
func FilterRequestHeaders(h http.Header) http.Header {
	out := h.Clone()
	for _, k := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive",
		"Proxy-Authenticate", "Proxy-Authorization", "Te",
		"Trailer", "Transfer-Encoding", "Upgrade",
	} {
		out.Del(k)
	}
	return out
}

// FilterResponseHeaders 过滤响应头，去除逐跳头和将由代理重新设置的头。
func FilterResponseHeaders(h http.Header) http.Header {
	out := h.Clone()
	for _, k := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive",
		"Trailer", "Transfer-Encoding", "Upgrade",
		"Content-Length", "Content-Encoding",
		"ETag", // 索引由我们按改写内容重算 ETag
	} {
		out.Del(k)
	}
	return out
}

// MaybeGunzip 若 data 为 gzip 则解压，否则原样返回。
func MaybeGunzip(data []byte) ([]byte, error) {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return data, nil
	}
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return data, nil
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		return data, nil
	}
	return out, nil
}

// DetectContentType 根据 body 嗅探修正 simple index 的 Content-Type（含 PEP 691 MIME）。
func DetectContentType(ct string, data []byte) string {
	base := strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(base, ';'); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	bad := base == "" ||
		base == "application/x-gzip" ||
		base == "application/gzip" ||
		base == "application/octet-stream" ||
		base == "binary/octet-stream"
	if !bad && (strings.Contains(base, "json") ||
		strings.Contains(base, "html") ||
		strings.Contains(base, "vnd.pypi.simple")) {
		if strings.Contains(base, "json") {
			return "application/vnd.pypi.simple.v1+json"
		}
		if strings.Contains(base, "vnd.pypi.simple.v1+html") {
			return "application/vnd.pypi.simple.v1+html"
		}
		return "text/html; charset=utf-8"
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return "text/html; charset=utf-8"
	}
	switch trimmed[0] {
	case '{', '[':
		return "application/vnd.pypi.simple.v1+json"
	default:
		return "text/html; charset=utf-8"
	}
}
