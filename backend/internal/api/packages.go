package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/livehl/mirrorhub/internal/cache"
	pypihandler "github.com/livehl/mirrorhub/internal/handlers/pypi"
)

const (
	defaultPackagePageSize = 20
	maxPackagePageSize     = 100
)

type packageFile struct {
	Key         string `json:"key"`
	Filename    string `json:"filename"`
	Version     string `json:"version"`
	Type        string `json:"type"`
	Size        int64  `json:"size"`
	SourceURL   string `json:"source_url"`
	ContentType string `json:"content_type"`
	CreatedAt   string `json:"created_at"`
	LastAccess  string `json:"last_access"`
	TTLSeconds  int    `json:"ttl_seconds"`
	Kind        string `json:"kind"`
}

type packageSummary struct {
	Name         string   `json:"name"`
	Versions     []string `json:"versions"` // 本地已有发行文件的版本
	FileCount    int      `json:"file_count"` // 发行文件数（不含索引）
	TotalSize    int64    `json:"total_size"` // 发行文件总大小
	HasIndex     bool     `json:"has_index"`
	LastAccess   string   `json:"last_access"`
	Cached       bool     `json:"cached"` // 是否已有 wheel/sdist 等发行文件
}

func (s *Server) listPackages(w http.ResponseWriter, r *http.Request) {
	platform := normalizePlatformQuery(r.URL.Query().Get("platform"))
	page := parsePositiveInt(r.URL.Query().Get("page"), 1)
	pageSize := parsePositiveInt(r.URL.Query().Get("page_size"), defaultPackagePageSize)
	if pageSize > maxPackagePageSize {
		pageSize = maxPackagePageSize
	}

	// 非 PyPI：浏览该平台本地缓存（允许空查询列出全部）
	if platform != "pypi" {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		out, total := s.listLocalPlatformPackages(platform, q, page, pageSize)
		writeJSON(w, http.StatusOK, map[string]any{
			"platform":           platform,
			"mode":               "local",
			"packages":           out,
			"total":              total,
			"page":               page,
			"page_size":          pageSize,
			"catalog_size":       total,
			"catalog_ready":      true,
			"catalog_refreshing": false,
			"catalog_error":      "",
		})
		return
	}

	q := pypihandler.NormalizeName(r.URL.Query().Get("q"))
	s.ensureCatalogLoaded()

	meta := s.catalogMetaJSON()
	meta["platform"] = "pypi"
	meta["mode"] = "catalog"
	meta["page"] = page
	meta["page_size"] = pageSize
	if q == "" {
		meta["packages"] = []packageSummary{}
		meta["total"] = 0
		writeJSON(w, http.StatusOK, meta)
		return
	}

	names, matchTotal, catalogSize := s.searchCatalog(q, page, pageSize)
	local := s.localPackageIndex()
	out := make([]packageSummary, 0, len(names))
	for _, name := range names {
		if loc, ok := local[name]; ok {
			out = append(out, loc)
			continue
		}
		out = append(out, packageSummary{Name: name, Cached: false})
	}
	meta["packages"] = out
	meta["total"] = matchTotal
	meta["catalog_size"] = catalogSize
	writeJSON(w, http.StatusOK, meta)
}

func (s *Server) getPackage(w http.ResponseWriter, r *http.Request) {
	platform := normalizePlatformQuery(r.URL.Query().Get("platform"))
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = strings.TrimSpace(chi.URLParam(r, "name"))
	}
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	if platform != "pypi" {
		writeJSON(w, http.StatusOK, s.getLocalPlatformPackage(platform, name))
		return
	}

	name = pypihandler.NormalizeName(name)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	wantUpstream := r.URL.Query().Get("upstream") != "0"

	localFiles := make([]packageFile, 0)
	var hasIndex bool
	var totalSize int64
	var packageCount int
	versionSet := map[string]struct{}{}
	indexVersionSet := map[string]struct{}{}

	for _, e := range s.cache.List() {
		info, ok := entryArtifact(e)
		if !ok || info.Name != name {
			continue
		}
		isIndex := e.Kind == "index" || info.Type == "index"
		if isIndex {
			hasIndex = true
			for _, v := range versionsFromIndexEntry(s.cache, e) {
				indexVersionSet[v] = struct{}{}
			}
		} else {
			packageCount++
			if info.Version != "" {
				versionSet[info.Version] = struct{}{}
			}
		}
		pf := toPackageFile(e, info)
		if pf.Filename == "" {
			pf.Filename = e.Key
		}
		localFiles = append(localFiles, pf)
		totalSize += e.Size
	}

	versions := make([]string, 0, len(versionSet))
	for v := range versionSet {
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool {
		return versionGreater(versions[i], versions[j])
	})
	indexVersions := make([]string, 0, len(indexVersionSet))
	for v := range indexVersionSet {
		indexVersions = append(indexVersions, v)
	}
	sort.Slice(indexVersions, func(i, j int) bool {
		return versionGreater(indexVersions[i], indexVersions[j])
	})
	sort.Slice(localFiles, func(i, j int) bool {
		if localFiles[i].Version == localFiles[j].Version {
			return localFiles[i].Filename < localFiles[j].Filename
		}
		return versionGreater(localFiles[i].Version, localFiles[j].Version)
	})

	out := map[string]any{
		"name":           name,
		"platform":       "pypi",
		"versions":       versions,
		"index_versions": indexVersions,
		"files":          localFiles,
		"file_count":     len(localFiles),
		"package_count":  packageCount,
		"total_size":     totalSize,
		"has_index":      hasIndex,
		"cached":         packageCount > 0,
		"upstream":       nil,
		"upstream_error": "",
	}

	if wantUpstream {
		upVers, err := s.fetchUpstreamVersions(r, name)
		if err != nil {
			out["upstream_error"] = err.Error()
		} else {
			out["upstream"] = map[string]any{
				"versions": upVers,
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deletePackageEntry(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		http.Error(w, "key required", http.StatusBadRequest)
		return
	}
	if err := s.cache.Delete(key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "key": key})
}

func (s *Server) searchCatalog(q string, page, pageSize int) (pageNames []string, matchTotal, catalogSize int) {
	s.catalogMu.RLock()
	catalogSize = len(s.catalogNames)
	s.catalogMu.RUnlock()

	ranked := s.rankedCatalogNames(q)
	matchTotal = len(ranked)
	if matchTotal == 0 && q != "" {
		// 搜索无结果：可能 catalog 中尚无此新包，触发延迟刷新
		s.triggerCatalogRefreshIfStale(30 * time.Minute)
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultPackagePageSize
	}
	start := (page - 1) * pageSize
	if start >= matchTotal {
		return []string{}, matchTotal, catalogSize
	}
	end := start + pageSize
	if end > matchTotal {
		end = matchTotal
	}
	pageNames = append([]string{}, ranked[start:end]...)
	return pageNames, matchTotal, catalogSize
}

func (s *Server) rankedCatalogNames(q string) []string {
	const searchCacheTTL = 30 * time.Second
	s.searchCacheMu.Lock()
	if e, ok := s.searchCache[q]; ok && time.Since(e.at) < searchCacheTTL {
		out := e.names
		s.searchCacheMu.Unlock()
		return out
	}
	s.searchCacheMu.Unlock()

	s.catalogMu.RLock()
	names := s.catalogNames
	nameSet := s.catalogNameSet
	s.catalogMu.RUnlock()

	var exact, prefix, contains []string
	if nameSet != nil {
		if _, ok := nameSet[q]; ok {
			exact = []string{q}
		}
	}
	// 前缀：有序切片二分
	i := sort.Search(len(names), func(i int) bool { return names[i] >= q })
	for ; i < len(names); i++ {
		if !strings.HasPrefix(names[i], q) {
			break
		}
		if names[i] == q {
			continue
		}
		prefix = append(prefix, names[i])
	}
	if len(q) >= 2 {
		for _, name := range names {
			if name == q || strings.HasPrefix(name, q) {
				continue
			}
			if strings.Contains(name, q) {
				contains = append(contains, name)
			}
		}
	}
	out := make([]string, 0, len(exact)+len(prefix)+len(contains))
	out = append(out, exact...)
	out = append(out, prefix...)
	out = append(out, contains...)

	s.searchCacheMu.Lock()
	s.searchCache[q] = searchCacheEntry{names: out, at: time.Now()}
	s.searchCacheMu.Unlock()
	return out
}

func parsePositiveInt(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return fallback
	}
	return n
}

func (s *Server) localPackageIndex() map[string]packageSummary {
	type agg struct {
		sum        packageSummary
		versionSet map[string]struct{}
		indexSize  int64
	}
	byName := map[string]*agg{}

	for _, e := range s.cache.List() {
		info, ok := entryArtifact(e)
		if !ok {
			continue
		}
		name := info.Name
		a, ok := byName[name]
		if !ok {
			a = &agg{
				sum:        packageSummary{Name: name},
				versionSet: map[string]struct{}{},
			}
			byName[name] = a
		}
		la := e.LastAccess.Format("2006-01-02 15:04:05")
		if a.sum.LastAccess == "" || la > a.sum.LastAccess {
			a.sum.LastAccess = la
		}
		isIndex := e.Kind == "index" || info.Type == "index"
		if isIndex {
			a.sum.HasIndex = true
			a.indexSize += e.Size
			continue
		}
		// 仅统计发行文件
		a.sum.Cached = true
		a.sum.FileCount++
		a.sum.TotalSize += e.Size
		if info.Version != "" {
			a.versionSet[info.Version] = struct{}{}
		}
	}

	out := make(map[string]packageSummary, len(byName))
	for name, a := range byName {
		vers := make([]string, 0, len(a.versionSet))
		for v := range a.versionSet {
			vers = append(vers, v)
		}
		sort.Slice(vers, func(i, j int) bool {
			return versionGreater(vers[i], vers[j])
		})
		a.sum.Versions = vers
		// 仅有索引、无发行文件时，用索引体积方便列表展示占用
		if !a.sum.Cached && a.sum.HasIndex {
			a.sum.TotalSize = a.indexSize
		}
		out[name] = a.sum
	}
	return out
}

func versionsFromIndexEntry(c *cache.Manager, e cache.Entry) []string {
	if c == nil || e.Key == "" {
		return nil
	}
	// 列表可能含过期索引：用 ReadAllStale 持锁读，避免 FilePath 裸读 TOCTOU
	_, body, ok := c.ReadAllStale(e.Key)
	if !ok || len(body) == 0 {
		return nil
	}
	refs := pypihandler.ExtractArtifactRefs(body, e.SourceURL)
	verSet := map[string]struct{}{}
	for _, ref := range refs {
		info := pypihandler.ParseArtifactURL(ref.URL, "package")
		if info.Version != "" {
			verSet[info.Version] = struct{}{}
		}
	}
	out := make([]string, 0, len(verSet))
	for v := range verSet {
		out = append(out, v)
	}
	return out
}

func (s *Server) fetchUpstreamVersions(r *http.Request, name string) ([]string, error) {
	if s.dl == nil {
		return nil, errNoDownloader
	}
	cfg := s.cfg.Get()
	pypi := cfg.Platforms["pypi"]
	indexURL := pypihandler.SimpleIndexURL(pypi.Upstream, name)
	body, _, err := s.dl.FetchSimpleIndex(r.Context(), indexURL, "pypi", cfg.Cache.IndexTTLSeconds, false)
	if err != nil {
		return nil, err
	}
	refs := pypihandler.ExtractArtifactRefs(body, indexURL)
	verSet := map[string]struct{}{}
	for _, ref := range refs {
		info := pypihandler.ParseArtifactURL(ref.URL, "package")
		if info.Version != "" {
			verSet[info.Version] = struct{}{}
		}
	}
	vers := make([]string, 0, len(verSet))
	for v := range verSet {
		vers = append(vers, v)
	}
	sort.Slice(vers, func(i, j int) bool {
		return versionGreater(vers[i], vers[j])
	})
	return vers, nil
}

func entryArtifact(e cache.Entry) (pypihandler.ArtifactInfo, bool) {
	if strings.TrimSpace(e.SourceURL) == "" || strings.TrimSpace(e.Kind) == "" {
		return pypihandler.ArtifactInfo{}, false
	}
	info := pypihandler.ParseArtifactURL(e.SourceURL, e.Kind)
	if info.Name == "" {
		return pypihandler.ArtifactInfo{}, false
	}
	return info, true
}

func toPackageFile(e cache.Entry, info pypihandler.ArtifactInfo) packageFile {
	return packageFile{
		Key:         e.Key,
		Filename:    info.Filename,
		Version:     info.Version,
		Type:        info.Type,
		Size:        e.Size,
		SourceURL:   e.SourceURL,
		ContentType: e.ContentType,
		CreatedAt:   e.CreatedAt.Format("2006-01-02 15:04:05"),
		LastAccess:  e.LastAccess.Format("2006-01-02 15:04:05"),
		TTLSeconds:  e.TTLSeconds,
		Kind:        e.Kind,
	}
}

func versionGreater(a, b string) bool {
	if a == "" {
		return false
	}
	if b == "" {
		return true
	}
	va, errA := pypihandler.ParseVersion(a)
	vb, errB := pypihandler.ParseVersion(b)
	if errA != nil || errB != nil {
		return a > b
	}
	return pypihandler.Compare(va, vb) > 0
}

type constError string

func (e constError) Error() string { return string(e) }

const errNoDownloader = constError("downloader unavailable")
