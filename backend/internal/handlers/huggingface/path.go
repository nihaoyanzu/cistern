// Package huggingface 提供 Hugging Face Hub 路径识别与仓库引用解析。
//
// 明确后置（不做）：终端用户登录透传、Spaces、上传、完整 Xet CAS 代理、git clone 协议。
package huggingface

import (
	"net/url"
	"strings"
)

// IsHuggingFacePath 是否为本平台应接管的 Hub 路径（models/datasets 的 api 与 resolve/raw）。
func IsHuggingFacePath(path string) bool {
	path = normalizePath(path)
	if strings.HasPrefix(path, "/spaces/") || strings.HasPrefix(path, "/api/spaces/") {
		return false
	}
	if IsAPIPath(path) {
		return true
	}
	return IsPackagePath(path)
}

// IsAPIPath Hub REST：/api/models/...、/api/datasets/...（含 tree、revision 等）。
func IsAPIPath(path string) bool {
	path = normalizePath(path)
	return strings.HasPrefix(path, "/api/models/") ||
		strings.HasPrefix(path, "/api/datasets/") ||
		path == "/api/models" ||
		path == "/api/datasets" ||
		strings.HasPrefix(path, "/api/resolve-cache/models/") ||
		strings.HasPrefix(path, "/api/resolve-cache/datasets/")
}

// IsPackagePath 文件下载：resolve / raw（含 datasets 前缀）。
func IsPackagePath(path string) bool {
	path = normalizePath(path)
	if strings.HasPrefix(path, "/spaces/") {
		return false
	}
	if strings.Contains(path, "/resolve/") {
		return strings.Index(path, "/resolve/") >= 1
	}
	if strings.Contains(path, "/raw/") {
		return strings.Index(path, "/raw/") >= 1
	}
	return false
}

// IsIndexPath 可变元数据（API）。
func IsIndexPath(path string) bool {
	return IsAPIPath(path)
}

// NormalizeETag 去掉引号与弱校验前缀。
func NormalizeETag(etag string) string {
	etag = strings.TrimSpace(etag)
	etag = strings.TrimPrefix(etag, "W/")
	etag = strings.Trim(etag, `"`)
	return strings.TrimSpace(etag)
}

// BlobCacheKeyFromETag 由内容寻址 ETag 生成 blob 键（诊断/兼容用）。
// 制品缓存键统一走 PackageCacheIdentity 的 hf:pkg:…，勿再按请求头切到本键。
func BlobCacheKeyFromETag(etag string) string {
	etag = NormalizeETag(etag)
	if etag == "" {
		return ""
	}
	return "hf:blob:" + etag
}

// StripXetHeaders 去掉 Xet 相关响应头，迫使客户端走经典 LFS/正文路径。
func StripXetHeaders(h map[string][]string) {
	if h == nil {
		return
	}
	for k := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-xet-") {
			delete(h, k)
		}
	}
	for _, key := range []string{"Link", "link"} {
		vals, ok := h[key]
		if !ok {
			continue
		}
		kept := make([]string, 0, len(vals))
		for _, v := range vals {
			lower := strings.ToLower(v)
			if strings.Contains(lower, "xet-auth") || strings.Contains(lower, "rel=\"xet") {
				continue
			}
			kept = append(kept, v)
		}
		if len(kept) == 0 {
			delete(h, key)
		} else {
			h[key] = kept
		}
	}
}

// DetectContentType API/索引 Content-Type。
func DetectContentType(path, upstreamCT string, body []byte) string {
	ct := strings.TrimSpace(strings.Split(upstreamCT, ";")[0])
	path = strings.ToLower(normalizePath(path))
	if strings.HasPrefix(path, "/api/") {
		if ct != "" && (strings.Contains(ct, "json") || strings.Contains(ct, "text")) {
			return upstreamCT
		}
		return "application/json"
	}
	if ct != "" {
		return upstreamCT
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return "application/json"
	}
	return "application/octet-stream"
}

// IsHuggingFaceArtifactURL 预取 URL 是否属于 HF。
func IsHuggingFaceArtifactURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Path == "" {
		return false
	}
	return IsHuggingFacePath(u.Path)
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
