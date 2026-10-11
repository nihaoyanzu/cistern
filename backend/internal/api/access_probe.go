package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/livehl/mirrorhub/internal/config"
	"github.com/livehl/mirrorhub/internal/platform"
)

type accessTestPlatformDraft struct {
	Enabled          bool   `json:"enabled"`
	Upstream         string `json:"upstream"`
	FileUpstream     string `json:"file_upstream"`
	MetadataUpstream string `json:"metadata_upstream"`
}

type accessTestReq struct {
	UpstreamProxy *string                            `json:"upstream_proxy"`
	Platforms     map[string]accessTestPlatformDraft `json:"platforms"`
}

type accessTestModule struct {
	ID     string                     `json:"id"`
	OK     bool                       `json:"ok"`
	Checks []platform.AccessProbeCheck `json:"checks"`
}

// postAccessTest 探测已开启模块上游；探测逻辑由各平台 AccessProber 实现。
func (s *Server) postAccessTest(w http.ResponseWriter, r *http.Request) {
	var body accessTestReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	cfg := s.cfg.Get()
	upstreamProxy := cfg.Server.UpstreamProxy
	if body.UpstreamProxy != nil {
		upstreamProxy = *body.UpstreamProxy
	}

	drafts := body.Platforms
	if len(drafts) == 0 {
		drafts = map[string]accessTestPlatformDraft{}
		for id, p := range cfg.Platforms {
			if !p.Enabled {
				continue
			}
			drafts[id] = accessTestPlatformDraft{
				Enabled:          true,
				Upstream:         p.Upstream,
				FileUpstream:     p.FileUpstream,
				MetadataUpstream: p.MetadataUpstream,
			}
		}
	}

	client := newAccessProbeClient(strings.TrimSpace(upstreamProxy), 12*time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	modules := make([]accessTestModule, 0)
	for _, p := range platform.All() {
		d, ok := drafts[p.Name()]
		if !ok || !d.Enabled {
			continue
		}
		prober, ok := p.(platform.AccessProber)
		if !ok {
			continue
		}
		pcfg := config.NormalizePlatform(p.Name(), config.PlatformConfig{
			Enabled:          true,
			Upstream:         d.Upstream,
			FileUpstream:     d.FileUpstream,
			MetadataUpstream: d.MetadataUpstream,
		})
		checks := prober.ProbeAccess(platform.AccessProbeEnv{
			Ctx:    ctx,
			Client: client,
			Cfg:    pcfg,
		})
		modOK := true
		for _, c := range checks {
			if !c.OK && !c.Skipped {
				modOK = false
				break
			}
		}
		modules = append(modules, accessTestModule{ID: p.Name(), OK: modOK, Checks: checks})
	}

	if strings.TrimSpace(upstreamProxy) != "" && len(modules) > 0 {
		anyOK := false
		for _, m := range modules {
			if m.OK {
				anyOK = true
				break
			}
		}
		modules[0].Checks = append(modules[0].Checks, platform.AccessProbeCheck{
			Name:   "upstream_proxy",
			OK:     anyOK,
			MS:     0,
			Detail: fmt.Sprintf("经由 %s（与各模块探测共用）", strings.TrimSpace(upstreamProxy)),
		})
	}

	allOK := len(modules) > 0
	for _, m := range modules {
		if !m.OK {
			allOK = false
			break
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      allOK,
		"modules": modules,
	})
}

func newAccessProbeClient(proxyURL string, timeout time.Duration) *http.Client {
	return platform.NewOutboundClient(proxyURL, timeout)
}
