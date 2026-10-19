// Package platform 定义平台抽象接口，使代理核心与具体平台（PyPI、npm、HuggingFace 等）解耦。
//
// 新增平台只需：
//  1. 实现 Platform 接口（及所需可选能力，见 capabilities.go）
//  2. 在 init() 中调用 platform.Register()
//  3. 在 proxy 包中以 blank import 引入
//
// 可选能力（LocalProber / UpstreamAuther / IndexKeyer / PrefetchExpander / TextDetector / AccessProber 等）
// 由编排层 type assert，勿在 proxy/prefetch/api 中按平台名字符串分支扩展。
package platform

import (
	"github.com/nihaoyanzu/cistern/internal/config"
	"github.com/nihaoyanzu/cistern/internal/router"
)

// IndexResult 是 TransformIndex 的输出。
type IndexResult struct {
	Body        []byte // 改写后的响应体（已可直接发给客户端）
	ETag        string // 内容摘要（带引号）
	ContentType string // 最终 Content-Type
}

// Platform 是一个包管理平台的抽象。
// 每个平台负责路由匹配和索引页改写；通用的缓存/限速/下载由代理层处理。
type Platform interface {
	// Name 返回平台标识，如 "pypi"、"huggingface"。
	// 同时用作 config.Config.Platforms 的 key 和缓存 key 前缀。
	Name() string

	// Route 根据请求路径判断是否属于本平台。
	// 返回 nil 表示不匹配，交给下一个平台处理。
	//
	// 返回的 router.Match 中：
	//   - Platform 必须设为 Name()
	//   - TargetURL 为上游完整 URL
	//   - Strategy 为代理策略（proxy/parallel）
	//   - IsIndex/IsMetadata/SmallFileBoost 按需设置
	Route(path string, cfg config.PlatformConfig) *router.Match

	// TransformIndex 改写上游索引页内容（含 Accept 协商，如 HTML→JSON）。
	// pageURL 为上游索引 URL，供 JSON name 等字段使用。
	// 通用的 gunzip 由调用方处理，此处接收已解码的内容。
	TransformIndex(body []byte, contentType string, cfg config.Config, accept, pageURL string) IndexResult
}
