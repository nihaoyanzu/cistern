package downloader

import (
	"context"
	"net/http"
	"strings"
	"sync"
)

// HubFileMeta 从上游（含重定向前的 Hub 响应）收集的 HF Hub 元数据头。
// hf CLI / huggingface_hub 依赖 X-Repo-Commit 判断是否为 Hub 兼容端点。
type HubFileMeta struct {
	mu         sync.Mutex
	RepoCommit string
	LinkedEtag string
	LinkedSize string
}

type hubMetaCtxKey struct{}

// WithHubFileMeta 在 ctx 中挂载可写的 Hub 元数据收集器；RoundTrip 会在重定向链上填充。
func WithHubFileMeta(ctx context.Context) (context.Context, *HubFileMeta) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m := HubFileMetaFrom(ctx); m != nil {
		return ctx, m
	}
	m := &HubFileMeta{}
	return context.WithValue(ctx, hubMetaCtxKey{}, m), m
}

// HubFileMetaFrom 取 ctx 上的收集器（可无）。
func HubFileMetaFrom(ctx context.Context) *HubFileMeta {
	if ctx == nil {
		return nil
	}
	m, _ := ctx.Value(hubMetaCtxKey{}).(*HubFileMeta)
	return m
}

func (m *HubFileMeta) merge(h http.Header) {
	if m == nil || h == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if v := strings.TrimSpace(h.Get("X-Repo-Commit")); v != "" {
		m.RepoCommit = v
	}
	if v := strings.TrimSpace(h.Get("X-Linked-Etag")); v != "" {
		m.LinkedEtag = v
	} else if v := strings.TrimSpace(h.Get("X-Linked-ETag")); v != "" {
		m.LinkedEtag = v
	}
	if v := strings.TrimSpace(h.Get("X-Linked-Size")); v != "" {
		m.LinkedSize = v
	}
}

func (m *HubFileMeta) snapshot() (commit, etag, size string) {
	if m == nil {
		return "", "", ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.RepoCommit, m.LinkedEtag, m.LinkedSize
}

// CopyFrom 把 src 已捕获的 Hub 头合并进来（用于 HEAD 探测结果注入后续 GET/stream ctx）。
func (m *HubFileMeta) CopyFrom(src *HubFileMeta) {
	if m == nil || src == nil || m == src {
		return
	}
	commit, etag, size := src.snapshot()
	m.mu.Lock()
	defer m.mu.Unlock()
	if commit != "" {
		m.RepoCommit = commit
	}
	if etag != "" {
		m.LinkedEtag = etag
	}
	if size != "" {
		m.LinkedSize = size
	}
}

// ApplyHubFileMetaHeaders 把 Hub 元数据写到客户端响应（优先收集器，其次最终响应头）。
func ApplyHubFileMetaHeaders(w http.ResponseWriter, meta *HubFileMeta, final http.Header) {
	if w == nil {
		return
	}
	commit, etag, size := meta.snapshot()
	if commit == "" && final != nil {
		commit = strings.TrimSpace(final.Get("X-Repo-Commit"))
	}
	if etag == "" && final != nil {
		etag = strings.TrimSpace(final.Get("X-Linked-Etag"))
		if etag == "" {
			etag = strings.TrimSpace(final.Get("X-Linked-ETag"))
		}
	}
	if size == "" && final != nil {
		size = strings.TrimSpace(final.Get("X-Linked-Size"))
	}
	if commit != "" {
		w.Header().Set("X-Repo-Commit", commit)
	}
	if etag != "" {
		w.Header().Set("X-Linked-Etag", etag)
	}
	if size != "" {
		w.Header().Set("X-Linked-Size", size)
	}
}

// hubCaptureRT 在重定向链的每一跳响应上收集 Hub 头（最终 CDN 响应通常没有这些头）。
type hubCaptureRT struct {
	base http.RoundTripper
}

func (t *hubCaptureRT) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if m := HubFileMetaFrom(req.Context()); m != nil {
		m.merge(resp.Header)
	}
	return resp, err
}

func wrapHubCapture(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	if _, ok := rt.(*hubCaptureRT); ok {
		return rt
	}
	return &hubCaptureRT{base: rt}
}

func unwrapHTTPTransport(rt http.RoundTripper) *http.Transport {
	switch t := rt.(type) {
	case *http.Transport:
		return t
	case *hubCaptureRT:
		if inner, ok := t.base.(*http.Transport); ok {
			return inner
		}
	}
	return nil
}
