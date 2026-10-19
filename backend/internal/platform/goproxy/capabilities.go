package goproxy

import (
	"net/http"

	"github.com/nihaoyanzu/cistern/internal/config"
	goproxyhandler "github.com/nihaoyanzu/cistern/internal/handlers/goproxy"
)

func (p *GoproxyPlatform) TryLocalProbe(w http.ResponseWriter, r *http.Request, cfg config.Config) bool {
	if !goproxyhandler.IsSumDBSupported(r.URL.Path) {
		return false
	}
	pcfg, ok := cfg.Platforms["goproxy"]
	if !ok || !pcfg.Enabled {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "goproxy mirror is read-only", http.StatusMethodNotAllowed)
	}
	return true
}

func (p *GoproxyPlatform) DetectIndexContentType(upstreamCT, _ string, body []byte) string {
	return goproxyhandler.DetectContentType("", upstreamCT, body)
}
