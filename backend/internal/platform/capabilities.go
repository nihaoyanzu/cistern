package platform

import (
	"context"
	"io"
	"net/http"

	"go.uber.org/zap"

	"github.com/nihaoyanzu/cistern/internal/cache"
	"github.com/nihaoyanzu/cistern/internal/config"
)

// LocalProber 本地短路探活（如 docker /v2、goproxy sumdb supported），不经 Match。
type LocalProber interface {
	// TryLocalProbe 若已写响应返回 true。
	TryLocalProbe(w http.ResponseWriter, r *http.Request, cfg config.Config) bool
}

// UpstreamAuther 回源请求注入鉴权。
type UpstreamAuther interface {
	InjectUpstreamAuth(ctx context.Context, headers http.Header, cfg config.Config, reqPath string) error
}

// UpstreamAuthInvalidator 上游 401 时清票；challenge 为响应头（可含 WWW-Authenticate），可为 nil。
type UpstreamAuthInvalidator interface {
	InvalidateUpstreamAuth(cfg config.Config, reqPath string, challenge http.Header)
}

// IndexKeyer 索引缓存键 Accept 变体后缀（不含前导冒号）；空串表示无后缀。
type IndexKeyer interface {
	IndexCacheVariant(accept string) string
}

// IndexAccepter 为回源索引请求准备 Accept 等头。
type IndexAccepter interface {
	PrepareIndexHeaders(clientAccept string, headers http.Header)
}

// IndexHeaderDecorator 索引响应额外头（如 Docker-Content-Digest）。
type IndexHeaderDecorator interface {
	DecorateIndexHeaders(w http.ResponseWriter, body []byte)
}

// PackageKeyer 制品缓存键与可选期望摘要。
type PackageKeyer interface {
	// PackageCacheIdentity 返回 cacheKey、expectedSHA256（空表示沿用 LookupDigest）。
	PackageCacheIdentity(reqPath, targetURL string, reqHeader http.Header, defaultKey string) (cacheKey, expectedSHA string)
}

// PackageHeaderDecorator 制品响应额外头（如 Docker-Distribution-API-Version）。
type PackageHeaderDecorator interface {
	DecoratePackageHeaders(w http.ResponseWriter)
}

// ContentTypeDetector 索引缓存用 Content-Type。
type ContentTypeDetector interface {
	DetectIndexContentType(upstreamCT, accept string, body []byte) string
}

// PrefetchBackend 预取展开所需 IO，由 prefetch 注入，避免 platform→downloader 循环依赖。
type PrefetchBackend interface {
	ProxyBytes(ctx context.Context, method, rawURL string, headers http.Header, body io.Reader, plat string, prefetch bool) (status int, header http.Header, data []byte, err error)
	PutBytes(key string, body []byte, contentType string, ttlSeconds int, meta cache.Meta) (*cache.Entry, error)
	FetchSimpleIndex(ctx context.Context, indexURL, plat string, ttlSeconds int, prefetch bool) (body []byte, contentType string, err error)
	FetchMetadata(ctx context.Context, metaURL, plat string, ttlSeconds int, prefetch bool) ([]byte, error)
	RememberDigest(rawURL, sha256hex string)
}

// PrefetchExpandEnv 预取展开上下文。
type PrefetchExpandEnv struct {
	Ctx     context.Context
	Cfg     config.Config
	Backend PrefetchBackend
	Log     *zap.Logger
}

// PrefetchExpander 将规格/坐标展开为待下载 URL。
// Owns 冲突时按 PrefetchPriority 降序（同优先级保持 Register 顺序）。
type PrefetchExpander interface {
	PrefetchPriority() int
	OwnsPrefetchItem(item string) bool
	ExpandPrefetchItem(env PrefetchExpandEnv, item string) ([]string, error)
}

// TextDetectResult 粘贴预取解析结果。
type TextDetectResult struct {
	Items   []string
	Skipped []string
	Kind    string // 如 npm_lock、docker_image，供审计
}

// TextDetector 管理面粘贴文本识别。
type TextDetector interface {
	// TextDetectPriority 越大越优先。
	TextDetectPriority() int
	LookLikePrefetchText(text string) bool
	ParsePrefetchText(text string) (TextDetectResult, bool)
}

// PrefetchTextResolver 粘贴解析可回源（如 Maven POM 沿 parent/BOM 补全版本）。
type PrefetchTextResolver interface {
	ResolvePrefetchText(env PrefetchExpandEnv, text string) (TextDetectResult, bool)
}

// AccessProbeCheck 单条上游探测结果。
type AccessProbeCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Detail  string `json:"detail"`
	MS      int64  `json:"ms"`
}

// AccessProbeEnv 管理面连通性探测上下文（Client 已含出站代理）。
type AccessProbeEnv struct {
	Ctx    context.Context
	Client *http.Client
	Cfg    config.PlatformConfig
}

// AccessProber 各平台自描述上游探测；api 层按 Register 顺序调用，勿按平台名分支。
type AccessProber interface {
	ProbeAccess(env AccessProbeEnv) []AccessProbeCheck
}
