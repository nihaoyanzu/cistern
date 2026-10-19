package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Manifest digests 提取用结构（list + image v2 / oci）。
type manifestDoc struct {
	MediaType     string `json:"mediaType"`
	SchemaVersion int    `json:"schemaVersion"`
	// image manifest
	Config *struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		Digest string `json:"digest"`
	} `json:"layers"`
	// list / index
	Manifests []struct {
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
		Platform  *struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	} `json:"manifests"`
}

// TargetArch 预取目标平台，如 linux/amd64。
type TargetArch struct {
	OS, Architecture, Variant string
}

// DefaultTargetArchs 由 Cistern target_platforms 映射。
func DefaultTargetArchs(platforms []string) []TargetArch {
	var out []TargetArch
	seen := map[string]struct{}{}
	add := func(os, arch, variant string) {
		key := os + "/" + arch + "/" + variant
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, TargetArch{OS: os, Architecture: arch, Variant: variant})
	}
	for _, p := range platforms {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "linux", "linux-amd64", "":
			add("linux", "amd64", "")
		case "linux-arm", "linux-arm64":
			add("linux", "arm64", "")
		case "win32", "windows":
			add("windows", "amd64", "")
		case "win-arm":
			add("windows", "arm64", "")
		case "darwin":
			add("linux", "amd64", "") // 容器镜像预取默认仍用 linux
		case "darwin-arm":
			add("linux", "arm64", "")
		default:
			add("linux", "amd64", "")
		}
	}
	if len(out) == 0 {
		add("linux", "amd64", "")
	}
	return out
}

// ManifestToCache 预取时需写入索引缓存的 manifest（tag 或 digest）。
type ManifestToCache struct {
	SourceURL   string
	Body        []byte
	ContentType string
}

// ResolveBlobURLs 拉取 manifest（含 list）并展开为上游 blob URL；同时返回需落盘的 manifest。
func ResolveBlobURLs(ctx context.Context, client *http.Client, tokens *TokenSource, registryBase string, ref ImageRef, archs []TargetArch) (blobURLs []string, manifests []ManifestToCache, err error) {
	if client == nil {
		client = http.DefaultClient
	}
	if tokens == nil {
		return nil, nil, fmt.Errorf("docker token source required")
	}
	// 与 docker 客户端一致：先请求资源，401 再按 WWW-Authenticate 换票（见 fetchManifest）
	body, ct, err := fetchManifest(ctx, client, tokens, registryBase, ref.Repo, ref.Tag, DefaultManifestAccept())
	if err != nil {
		return nil, nil, err
	}
	manifests = append(manifests, ManifestToCache{
		SourceURL:   RegistryManifestURL(registryBase, ref.Repo, ref.Tag),
		Body:        body,
		ContentType: DetectManifestContentType(ct, body),
	})
	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, nil, err
	}
	media := doc.MediaType
	if media == "" {
		media = ct
	}

	digests := map[string]struct{}{}
	addDigest := func(d string) {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			return
		}
		digests[d] = struct{}{}
	}

	if isManifestList(media) || len(doc.Manifests) > 0 {
		selected := selectManifestDigests(doc, archs)
		if len(selected) == 0 {
			return nil, nil, fmt.Errorf("无匹配架构的 manifest（arch=%v）", archs)
		}
		for _, dig := range selected {
			child, childCT, err := fetchManifest(ctx, client, tokens, registryBase, ref.Repo, dig, DefaultManifestAccept())
			if err != nil {
				return nil, nil, fmt.Errorf("manifest %s: %w", dig, err)
			}
			manifests = append(manifests, ManifestToCache{
				SourceURL:   RegistryManifestURL(registryBase, ref.Repo, dig),
				Body:        child,
				ContentType: DetectManifestContentType(childCT, child),
			})
			var childDoc manifestDoc
			if err := json.Unmarshal(child, &childDoc); err != nil {
				return nil, nil, err
			}
			collectImageDigests(childDoc, addDigest)
		}
	} else {
		collectImageDigests(doc, addDigest)
	}

	out := make([]string, 0, len(digests))
	for d := range digests {
		out = append(out, RegistryBlobURL(registryBase, ref.Repo, d))
	}
	return out, manifests, nil
}

func collectImageDigests(doc manifestDoc, add func(string)) {
	if doc.Config != nil {
		add(doc.Config.Digest)
	}
	for _, l := range doc.Layers {
		add(l.Digest)
	}
}

func isManifestList(media string) bool {
	m := strings.ToLower(media)
	return strings.Contains(m, "manifest.list") || strings.Contains(m, "image.index")
}

func selectManifestDigests(doc manifestDoc, archs []TargetArch) []string {
	want := map[string]struct{}{}
	for _, a := range archs {
		key := strings.ToLower(a.OS) + "/" + strings.ToLower(a.Architecture)
		if a.Variant != "" {
			key += "/" + strings.ToLower(a.Variant)
		}
		want[key] = struct{}{}
		// 也接受无 variant 的匹配
		want[strings.ToLower(a.OS)+"/"+strings.ToLower(a.Architecture)] = struct{}{}
	}
	var out []string
	for _, m := range doc.Manifests {
		if m.Digest == "" || m.Platform == nil {
			continue
		}
		key := strings.ToLower(m.Platform.OS) + "/" + strings.ToLower(m.Platform.Architecture)
		keyVar := key
		if m.Platform.Variant != "" {
			keyVar += "/" + strings.ToLower(m.Platform.Variant)
		}
		if _, ok := want[keyVar]; ok {
			out = append(out, m.Digest)
			continue
		}
		if _, ok := want[key]; ok {
			out = append(out, m.Digest)
		}
	}
	return out
}

func fetchManifest(ctx context.Context, client *http.Client, tokens *TokenSource, registryBase, repo, reference, accept string) ([]byte, string, error) {
	u := RegistryManifestURL(registryBase, repo, reference)
	do := func(withAuth bool) (status int, hdr http.Header, body []byte, err error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return 0, nil, nil, err
		}
		req.Header.Set("Accept", accept)
		if withAuth {
			tok, err := tokens.Bearer(ctx, repo)
			if err != nil {
				return 0, nil, nil, err
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		body, err = io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil {
			return resp.StatusCode, resp.Header.Clone(), nil, err
		}
		return resp.StatusCode, resp.Header.Clone(), body, nil
	}

	// 已学到 realm 则带票；否则匿名首试（对齐 docker 客户端）
	status, hdr, body, err := do(tokens.HasRealm())
	if err != nil {
		return nil, "", err
	}
	if status == http.StatusUnauthorized {
		if ch, ok := ParseBearerChallenge(hdr); ok {
			tokens.ApplyChallenge(ch)
		} else if tokens.HasRealm() {
			tokens.Invalidate(repo)
		} else {
			return nil, "", fmt.Errorf("manifest %s: missing Bearer challenge", http.StatusText(status))
		}
		status, hdr, body, err = do(true)
		if err != nil {
			return nil, "", err
		}
	}
	if status >= 400 {
		return nil, "", fmt.Errorf("manifest %s: %s", http.StatusText(status), truncate(string(body), 200))
	}
	return body, hdr.Get("Content-Type"), nil
}
