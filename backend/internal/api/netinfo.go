package api

import (
	"net"
	"net/http"
	"strings"

	"github.com/nihaoyanzu/cistern/internal/platform"
)

// getPublicGuide 无需登录：返回全部已注册模块的启用状态与下载端口，
// 供说明页仅对已开启模块展开接入说明。
func (s *Server) getPublicGuide(w http.ResponseWriter, _ *http.Request) {
	cfg := s.cfg.Get()
	port := listenPort(cfg.Server.ProxyAddr)
	all := platform.All()
	modules := make([]map[string]any, 0, len(all))
	for _, p := range all {
		name := p.Name()
		enabled := false
		if pc, ok := cfg.Platforms[name]; ok {
			enabled = pc.Enabled
		}
		modules = append(modules, map[string]any{
			"id":      name,
			"enabled": enabled,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"proxy_port": port,
		"modules":    modules,
	})
}

func listenPort(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "8081"
	}
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return port
	}
	if strings.HasPrefix(addr, ":") {
		p := strings.TrimPrefix(addr, ":")
		if p != "" {
			return p
		}
	}
	return "8081"
}
