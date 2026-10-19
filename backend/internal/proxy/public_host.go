package proxy

import (
	"net/http"
	"strings"

	"github.com/nihaoyanzu/cistern/internal/config"
)

// applyRequestPublicHost 用客户端实际访问的 Host 改写索引里的下载链接。
func applyRequestPublicHost(cfg config.Config, r *http.Request) config.Config {
	reqHost := strings.TrimSpace(r.Host)
	if reqHost == "" {
		return cfg
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if xp := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); xp != "" {
		scheme = strings.ToLower(strings.TrimSpace(strings.Split(xp, ",")[0]))
		if scheme != "http" && scheme != "https" {
			scheme = "http"
		}
	}
	cfg.Server.PublicHost = scheme + "://" + reqHost
	return cfg
}
