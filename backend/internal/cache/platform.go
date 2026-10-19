package cache

import (
	"strings"

	"github.com/nihaoyanzu/cistern/internal/platid"
)

// knownPlatforms 与 platid.Order 一致，供外部枚举。
var knownPlatforms = append([]string(nil), platid.Order...)

// ClassifyPlatform 根据缓存 key / SourceURL / Kind 归类平台；无法识别返回 "other"。
// 与包列表同源（platid），单次 Stats 扫描即可得到正确 by_platform。
func ClassifyPlatform(e Entry) string {
	return platid.Of(e.Key, e.SourceURL, e.Kind)
}

// HitPlatform 命中率归属：优先用调用方传入的平台（路由已知），否则从条目/key 推断。
// 避免 cache.Get 依赖 proxy/platform 包，由上层解耦传入 hint。
func HitPlatform(hint, key string, entry *Entry) string {
	if p := strings.TrimSpace(hint); p != "" {
		return p
	}
	if entry != nil {
		return ClassifyPlatform(*entry)
	}
	return platid.Of(key, "", "")
}

// KnownPlatforms 返回 Stats 汇总时使用的平台顺序。
func KnownPlatforms() []string {
	out := make([]string, len(knownPlatforms))
	copy(out, knownPlatforms)
	return out
}
