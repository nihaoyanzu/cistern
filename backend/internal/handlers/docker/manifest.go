package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/nihaoyanzu/cistern/internal/config"
)

// IndexETag 由 manifest 正文生成。
func IndexETag(raw []byte) string {
	sum := sha256.Sum256(raw)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// DetectManifestContentType 保留上游 OCI/Docker manifest MIME。
func DetectManifestContentType(contentType string, body []byte) string {
	ct := strings.TrimSpace(strings.Split(contentType, ";")[0])
	if ct != "" && (strings.Contains(ct, "json") || strings.Contains(ct, "manifest") || strings.Contains(ct, "oci")) {
		return ct
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") {
		var probe struct {
			MediaType     string `json:"mediaType"`
			SchemaVersion int    `json:"schemaVersion"`
		}
		if json.Unmarshal(body, &probe) == nil && probe.MediaType != "" {
			return probe.MediaType
		}
		return "application/vnd.docker.distribution.manifest.v2+json"
	}
	if contentType != "" {
		return contentType
	}
	return "application/octet-stream"
}

// PublicBaseURL 与其它平台一致。
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

// AuthServiceFromRegistry 从 registry URL 推断 token service 名。
func AuthServiceFromRegistry(registryURL string) string {
	u := strings.ToLower(strings.TrimSpace(registryURL))
	if strings.Contains(u, "docker.io") {
		return "registry.docker.io"
	}
	raw := registryURL
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		return parsed.Hostname()
	}
	return "registry.docker.io"
}
