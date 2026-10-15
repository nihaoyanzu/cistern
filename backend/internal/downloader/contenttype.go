package downloader

import (
	"strconv"
	"strings"
)

func isHTMLContentType(ct string) bool {
	return strings.Contains(strings.ToLower(ct), "text/html")
}

// looksLikeDockerNonBlobJSON 粗判 registry 错误页或 manifest（不应作为 layer blob）。
func looksLikeDockerNonBlobJSON(ct string) bool {
	low := strings.ToLower(strings.TrimSpace(ct))
	if low == "" || !strings.Contains(low, "json") {
		return false
	}
	return strings.Contains(low, "manifest") ||
		strings.Contains(low, "vnd.docker.distribution.manifest") ||
		strings.Contains(low, "vnd.oci.image") ||
		strings.Contains(low, "error")
}

// parseLinkedSize 解析 HuggingFace X-Linked-Size 头；无效返回 -1。
func parseLinkedSize(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return -1
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}
