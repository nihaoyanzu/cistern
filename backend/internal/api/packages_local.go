package api

import (
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/nihaoyanzu/cistern/internal/cache"
	dockerhandler "github.com/nihaoyanzu/cistern/internal/handlers/docker"
	goproxyhandler "github.com/nihaoyanzu/cistern/internal/handlers/goproxy"
	hfhandler "github.com/nihaoyanzu/cistern/internal/handlers/huggingface"
	mavenhandler "github.com/nihaoyanzu/cistern/internal/handlers/maven"
	npmhandler "github.com/nihaoyanzu/cistern/internal/handlers/npm"
	pypihandler "github.com/nihaoyanzu/cistern/internal/handlers/pypi"
	"github.com/nihaoyanzu/cistern/internal/platid"
)

type localItem struct {
	Name     string
	Version  string
	Filename string
	Type     string
	Entry    cache.Entry
}

func normalizePlatformQuery(raw string) string {
	p := strings.ToLower(strings.TrimSpace(raw))
	switch p {
	case "", "pypi":
		return "pypi"
	case "huggingface", "hf":
		return "huggingface"
	case "goproxy", "go":
		return "goproxy"
	case "docker":
		return "docker"
	case "maven":
		return "maven"
	case "npm":
		return "npm"
	default:
		return p
	}
}

func entryBelongsToPlatform(e cache.Entry, platform string) bool {
	return platid.Belongs(platform, e.Key, e.SourceURL, e.Kind)
}

// platformOfEntry 与缓存列表 / Stats.by_platform 同源归属。
func platformOfEntry(e cache.Entry) string {
	return platid.Of(e.Key, e.SourceURL, e.Kind)
}

func localIdentity(platform string, e cache.Entry) (name, version, filename, typ string, ok bool) {
	src := strings.TrimSpace(e.SourceURL)
	key := e.Key
	typ = e.Kind
	if typ == "" {
		typ = "other"
	}
	filename = path.Base(src)
	if filename == "." || filename == "/" {
		filename = key
	}

	switch platform {
	case "huggingface":
		if u, err := url.Parse(src); err == nil && u.Path != "" {
			if ref, parsed := hfhandler.ParseRepoRef(u.Path); parsed {
				name = ref.ID
				if ref.RepoType == "datasets" || strings.HasPrefix(strings.Trim(u.Path, "/"), "datasets/") {
					name = "datasets/" + ref.ID
				}
				version = ref.Revision
				ok = name != ""
				return
			}
			// /owner/repo/resolve/...
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) >= 2 {
				if parts[0] == "datasets" && len(parts) >= 3 {
					name = "datasets/" + parts[1] + "/" + parts[2]
				} else if parts[0] == "api" {
					// /api/models/owner/repo/tree/...
					if len(parts) >= 4 && (parts[1] == "models" || parts[1] == "datasets") {
						name = parts[2] + "/" + parts[3]
						if parts[1] == "datasets" {
							name = "datasets/" + name
						}
					}
				} else {
					name = parts[0] + "/" + parts[1]
				}
				ok = name != ""
				return
			}
		}
		return
	case "goproxy":
		if u, err := url.Parse(src); err == nil {
			p := strings.Trim(u.Path, "/")
			// /module/@v/version.zip
			if i := strings.Index(p, "/@v/"); i > 0 {
				name = p[:i]
				rest := p[i+4:]
				version = strings.TrimSuffix(strings.TrimSuffix(rest, ".zip"), ".info")
				version = strings.TrimSuffix(version, ".mod")
				ok = name != ""
				return
			}
			if ref, parsed := goproxyhandler.ParseModuleRef(src); parsed {
				name = ref.Path
				version = ref.Version
				ok = true
				return
			}
		}
		return
	case "docker":
		if u, err := url.Parse(src); err == nil {
			if repo, ref, okm := dockerhandler.ParseManifestPath(u.Path); okm {
				name = repo
				version = ref
				ok = true
				return
			}
			if repo, dig, okb := dockerhandler.ParseBlobPath(u.Path); okb {
				name = repo
				version = dig
				typ = "blob"
				ok = true
				return
			}
		}
		if strings.HasPrefix(key, "docker:blob:") {
			name = "blob"
			version = strings.TrimPrefix(key, "docker:blob:")
			ok = true
			return
		}
		return
	case "maven":
		if u, err := url.Parse(src); err == nil && u.Path != "" {
			g, a, ver, isMeta, okm := mavenhandler.LayoutIdentity(u.Path)
			if okm {
				name = g + ":" + a
				version = ver
				if isMeta {
					typ = "index"
				}
				ok = true
				return
			}
		}
		if c, parsed := mavenhandler.ParseCoordinate(src); parsed {
			name = c.GroupID + ":" + c.ArtifactID
			version = c.Version
			ok = true
			return
		}
		return
	case "npm":
		if u, err := url.Parse(src); err == nil {
			p := u.Path
			if npmhandler.IsTarballPath(p) {
				// /pkg/-/pkg-ver.tgz or /@scope/pkg/-/pkg-ver.tgz
				base := path.Base(p)
				name = npmNameFromTarballPath(p)
				version = npmVersionFromFilename(base)
				ok = name != ""
				return
			}
			segs := strings.Split(strings.Trim(p, "/"), "/")
			if len(segs) >= 1 {
				if strings.HasPrefix(segs[0], "@") && len(segs) >= 2 {
					name = segs[0] + "/" + segs[1]
				} else {
					name = segs[0]
				}
				ok = name != ""
				return
			}
		}
		return
	default:
		info, ok2 := entryArtifact(e)
		if !ok2 {
			return
		}
		return info.Name, info.Version, info.Filename, info.Type, true
	}
}

func npmNameFromTarballPath(p string) string {
	p = strings.Trim(p, "/")
	if i := strings.Index(p, "/-/"); i > 0 {
		return p[:i]
	}
	return ""
}

func npmVersionFromFilename(base string) string {
	base = strings.TrimSuffix(base, ".tgz")
	// pkg-1.2.3 or pkg-name-1.2.3 — take last semver-ish segment after last -
	if i := strings.LastIndex(base, "-"); i > 0 && i+1 < len(base) {
		return base[i+1:]
	}
	return ""
}

func (s *Server) listLocalPlatformPackages(platform, q string, page, pageSize int) (out []packageSummary, total int) {
	q = strings.ToLower(strings.TrimSpace(q))
	type agg struct {
		sum        packageSummary
		versionSet map[string]struct{}
	}
	byName := map[string]*agg{}

	for _, e := range s.cache.List() {
		if !entryBelongsToPlatform(e, platform) {
			continue
		}
		name, ver, _, typ, ok := localIdentity(platform, e)
		if !ok || name == "" {
			continue
		}
		a, exists := byName[name]
		if !exists {
			a = &agg{
				sum:        packageSummary{Name: name, Cached: true},
				versionSet: map[string]struct{}{},
			}
			byName[name] = a
		}
		la := e.LastAccess.Format("2006-01-02 15:04:05")
		if a.sum.LastAccess == "" || la > a.sum.LastAccess {
			a.sum.LastAccess = la
		}
		isIndex := e.Kind == "index" || typ == "index"
		if isIndex {
			a.sum.HasIndex = true
			// Docker manifest 的 reference 即 tag/digest；其它平台 index 不计版本
			if platform == "docker" && ver != "" {
				a.versionSet[ver] = struct{}{}
			}
			continue
		}
		a.sum.FileCount++
		a.sum.TotalSize += e.Size
		// Docker blob 层的 digest 不是镜像版本
		if platform == "docker" && (typ == "blob" || strings.HasPrefix(ver, "sha256:")) {
			continue
		}
		if ver != "" {
			a.versionSet[ver] = struct{}{}
		}
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		if q != "" && !strings.Contains(strings.ToLower(name), q) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	total = len(names)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultPackagePageSize
	}
	start := (page - 1) * pageSize
	if start >= total {
		return []packageSummary{}, total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	out = make([]packageSummary, 0, end-start)
	for _, name := range names[start:end] {
		a := byName[name]
		vers := make([]string, 0, len(a.versionSet))
		for v := range a.versionSet {
			vers = append(vers, v)
		}
		a.sum.Versions = finalizeLocalVersions(platform, vers)
		out = append(out, a.sum)
	}
	return out, total
}

func (s *Server) getLocalPlatformPackage(platform, name string) map[string]any {
	name = strings.TrimSpace(name)
	localFiles := make([]packageFile, 0)
	var totalSize int64
	var packageCount int
	versionSet := map[string]struct{}{}
	var hasIndex bool

	for _, e := range s.cache.List() {
		if !entryBelongsToPlatform(e, platform) {
			continue
		}
		n, ver, filename, typ, ok := localIdentity(platform, e)
		if !ok || !localNameEqual(platform, n, name) {
			continue
		}
		isIndex := e.Kind == "index" || typ == "index"
		if isIndex {
			hasIndex = true
			if platform == "docker" && ver != "" {
				versionSet[ver] = struct{}{}
			}
		} else {
			packageCount++
			if ver != "" && !(platform == "docker" && (typ == "blob" || strings.HasPrefix(ver, "sha256:"))) {
				versionSet[ver] = struct{}{}
			}
		}
		pf := packageFile{
			Key:         e.Key,
			Filename:    filename,
			Version:     ver,
			Type:        typ,
			Size:        e.Size,
			SourceURL:   e.SourceURL,
			ContentType: e.ContentType,
			CreatedAt:   e.CreatedAt.Format("2006-01-02 15:04:05"),
			LastAccess:  e.LastAccess.Format("2006-01-02 15:04:05"),
			TTLSeconds:  e.TTLSeconds,
			Kind:        e.Kind,
		}
		localFiles = append(localFiles, pf)
		totalSize += e.Size
	}

	versions := make([]string, 0, len(versionSet))
	for v := range versionSet {
		versions = append(versions, v)
	}
	versions = finalizeLocalVersions(platform, versions)
	sort.Slice(localFiles, func(i, j int) bool {
		if localFiles[i].Version == localFiles[j].Version {
			return localFiles[i].Filename < localFiles[j].Filename
		}
		return localFiles[i].Version > localFiles[j].Version
	})

	return map[string]any{
		"name":           name,
		"platform":       platform,
		"versions":       versions,
		"index_versions": []string{},
		"files":          localFiles,
		"file_count":     len(localFiles),
		"package_count":  packageCount,
		"total_size":     totalSize,
		"has_index":      hasIndex,
		"cached":         packageCount > 0 || hasIndex,
		"upstream":       nil,
		"upstream_error": "",
	}
}

func localNameEqual(platform, a, b string) bool {
	if platform == "pypi" {
		return pypihandler.NormalizeName(a) == pypihandler.NormalizeName(b)
	}
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// finalizeLocalVersions Docker：有 tag 时不把 digest 当版本（同内容常被 tag+digest 各缓存一份）；仅 digest 拉取时才保留。
func finalizeLocalVersions(platform string, vers []string) []string {
	if platform != "docker" {
		sort.Strings(vers)
		return vers
	}
	tags := make([]string, 0, len(vers))
	digests := make([]string, 0, len(vers))
	for _, v := range vers {
		if strings.HasPrefix(v, "sha256:") {
			digests = append(digests, v)
		} else if v != "" {
			tags = append(tags, v)
		}
	}
	if len(tags) > 0 {
		sort.Strings(tags)
		return tags
	}
	sort.Strings(digests)
	return digests
}
