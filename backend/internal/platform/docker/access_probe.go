package docker

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	dockerhandler "github.com/livehl/mirrorhub/internal/handlers/docker"
	"github.com/livehl/mirrorhub/internal/platform"
)

func (p *DockerPlatform) ProbeAccess(env platform.AccessProbeEnv) []platform.AccessProbeCheck {
	base := strings.TrimRight(strings.TrimSpace(env.Cfg.Upstream), "/")
	if base == "" {
		return []platform.AccessProbeCheck{{
			Name: "manifest", OK: false, MS: 0,
			Detail: "未配置上游",
		}}
	}
	const repo = "library/hello-world"
	const tag = "latest"
	tokens := dockerhandler.NewTokenSource(dockerhandler.AuthServiceFromRegistry(base), env.Client)

	t0 := time.Now()
	body, manifestURL, err := fetchProbeManifest(env, tokens, base, repo, tag)
	ms := time.Since(t0).Milliseconds()
	if err != nil {
		return []platform.AccessProbeCheck{{
			Name: "manifest", OK: false, MS: ms,
			Detail: fmt.Sprintf("%s: %v", manifestURL, err),
		}}
	}

	// manifest list → 再拉第一个子 manifest，才能拿到 layer digest
	if dig := firstListChildDigest(body); dig != "" {
		child, childURL, err := fetchProbeManifest(env, tokens, base, repo, dig)
		if err == nil && len(child) > 0 {
			body = child
			manifestURL = childURL
		}
	}

	checks := []platform.AccessProbeCheck{{
		Name: "manifest", OK: true, MS: ms,
		Detail: fmt.Sprintf("%s → 已拉取 manifest %d 字节", manifestURL, len(body)),
	}}

	digest := firstBlobDigest(body)
	if digest == "" {
		checks = append(checks, platform.AccessProbeCheck{
			Name: "download", OK: false, MS: 0,
			Detail: "manifest 中未找到 layer/config digest",
		})
		return checks
	}
	blobURL := dockerhandler.RegistryBlobURL(base, repo, digest)
	hdr := http.Header{}
	if tok, err := tokens.Bearer(env.Ctx, repo); err == nil && tok != "" {
		hdr.Set("Authorization", "Bearer "+tok)
	}
	checks = append(checks, platform.ProbeDownloadSample(env.Ctx, env.Client, "download", blobURL, hdr))
	return checks
}

func fetchProbeManifest(env platform.AccessProbeEnv, tokens *dockerhandler.TokenSource, registryBase, repo, reference string) ([]byte, string, error) {
	u := dockerhandler.RegistryManifestURL(registryBase, repo, reference)
	try := func(withAuth bool) (int, http.Header, []byte, error) {
		req, err := http.NewRequestWithContext(env.Ctx, http.MethodGet, u, nil)
		if err != nil {
			return 0, nil, nil, err
		}
		req.Header.Set("User-Agent", "mirrorhub-access-test/1.0")
		req.Header.Set("Accept", dockerhandler.DefaultManifestAccept())
		if withAuth {
			if tok, err := tokens.Bearer(env.Ctx, repo); err == nil && tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		resp, err := env.Client.Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		if err != nil {
			return resp.StatusCode, resp.Header.Clone(), nil, err
		}
		return resp.StatusCode, resp.Header.Clone(), body, nil
	}

	// 已有 realm 带票，否则匿名首试
	status, hdr, body, err := try(tokens.HasRealm())
	if err != nil {
		return nil, u, err
	}
	if status == http.StatusUnauthorized {
		if ch, ok := dockerhandler.ParseBearerChallenge(hdr); ok {
			tokens.ApplyChallenge(ch)
		} else if tokens.HasRealm() {
			tokens.Invalidate(repo)
		} else {
			return nil, u, fmt.Errorf("HTTP 401 without Bearer challenge")
		}
		status, hdr, body, err = try(true)
		if err != nil {
			return nil, u, err
		}
		_ = hdr
	}
	if status >= 400 {
		return nil, u, fmt.Errorf("HTTP %d", status)
	}
	if len(body) == 0 {
		return nil, u, fmt.Errorf("空 manifest")
	}
	return body, u, nil
}

func firstListChildDigest(manifest []byte) string {
	var doc struct {
		Manifests []struct {
			Digest    string `json:"digest"`
			MediaType string `json:"mediaType"`
		} `json:"manifests"`
	}
	if json.Unmarshal(manifest, &doc) != nil || len(doc.Manifests) == 0 {
		return ""
	}
	for _, m := range doc.Manifests {
		mt := strings.ToLower(m.MediaType)
		if strings.Contains(mt, "manifest.list") || strings.Contains(mt, "image.index") {
			continue
		}
		if d := strings.TrimSpace(m.Digest); d != "" {
			return d
		}
	}
	return strings.TrimSpace(doc.Manifests[0].Digest)
}

func firstBlobDigest(manifest []byte) string {
	var doc struct {
		Config *struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if json.Unmarshal(manifest, &doc) != nil {
		return ""
	}
	for _, l := range doc.Layers {
		if d := strings.TrimSpace(l.Digest); d != "" {
			return d
		}
	}
	if doc.Config != nil {
		if d := strings.TrimSpace(doc.Config.Digest); d != "" {
			return d
		}
	}
	return ""
}
