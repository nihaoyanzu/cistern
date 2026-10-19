package api

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	pypihandler "github.com/nihaoyanzu/cistern/internal/handlers/pypi"
)

// 包名列表独立于索引 HTML 缓存：落盘后重启可直接用，到期再后台刷新
const (
	catalogTTL          = 6 * time.Hour
	catalogCheckEvery   = 5 * time.Minute
	catalogPublishChunk = 5000
	catalogFileName     = "pypi-names.txt"
	catalogFileMagic    = "# cistern-catalog-v1"
	catalogFileMagicOld = "# mirrorhub-catalog-v1"
)

func (s *Server) catalogDir() string {
	return filepath.Join(s.cfg.Get().Cache.Dir, "catalog")
}

func (s *Server) catalogPath() string {
	return filepath.Join(s.catalogDir(), catalogFileName)
}

func (s *Server) catalogRootURL() string {
	pypi := s.cfg.Get().Platforms["pypi"]
	return strings.TrimRight(pypi.Upstream, "/") + "/simple/"
}

// StartCatalog 启动时加载落盘索引，并定时/按需后台刷新
func (s *Server) StartCatalog(ctx context.Context) {
	root := s.catalogRootURL()
	if n, at, url, err := s.loadCatalogFile(); err == nil && len(n) > 0 {
		if url == "" || url == root {
			s.setCatalog(n, root, at, false, "")
			s.log.Info("loaded package catalog from disk",
				zap.Int("names", len(n)),
				zap.Time("updated_at", at),
			)
		} else {
			s.log.Info("disk catalog upstream mismatch, will refresh",
				zap.String("disk", url),
				zap.String("current", root),
			)
		}
	}
	s.kickCatalogRefresh(false)

	go func() {
		t := time.NewTicker(catalogCheckEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.kickCatalogRefresh(false)
			}
		}
	}()
}

func (s *Server) catalogInfo() (size int, ready bool, refreshing bool, updatedAt time.Time, lastErr string) {
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()
	return len(s.catalogNames), len(s.catalogNames) > 0, s.catalogRefreshing, s.catalogAt, s.catalogLastErr
}

func (s *Server) catalogMetaJSON() map[string]any {
	size, ready, refreshing, at, lastErr := s.catalogInfo()
	out := map[string]any{
		"catalog_size":       size,
		"catalog_ready":      ready,
		"catalog_refreshing": refreshing,
		"catalog_error":      lastErr,
	}
	if !at.IsZero() {
		out["catalog_updated_at"] = at.Format(time.RFC3339)
	}
	return out
}

func (s *Server) setCatalog(names []string, url string, at time.Time, refreshing bool, lastErr string) {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	s.catalogMu.Lock()
	s.catalogNames = names
	s.catalogNameSet = set
	s.catalogURL = url
	s.catalogAt = at
	s.catalogRefreshing = refreshing
	if lastErr != "" || !refreshing {
		s.catalogLastErr = lastErr
	}
	s.catalogMu.Unlock()
	s.clearSearchCache()
}

func (s *Server) publishCatalogChunk(names []string, url string, refreshing bool) {
	cp := make([]string, len(names))
	copy(cp, names)
	set := make(map[string]struct{}, len(cp))
	for _, n := range cp {
		set[n] = struct{}{}
	}
	s.catalogMu.Lock()
	s.catalogNames = cp
	s.catalogNameSet = set
	s.catalogURL = url
	s.catalogRefreshing = refreshing
	s.catalogMu.Unlock()
	s.clearSearchCache()
}

func (s *Server) clearSearchCache() {
	s.searchCacheMu.Lock()
	s.searchCache = map[string]searchCacheEntry{}
	s.searchCacheMu.Unlock()
}

func (s *Server) catalogNeedsRefresh() bool {
	root := s.catalogRootURL()
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()
	if s.catalogRefreshing {
		return false
	}
	if len(s.catalogNames) == 0 {
		return true
	}
	if s.catalogURL != "" && s.catalogURL != root {
		return true
	}
	if s.catalogAt.IsZero() {
		return true
	}
	return time.Since(s.catalogAt) >= catalogTTL
}

// kickCatalogRefresh 非阻塞触发后台刷新；force 时即使未到期也刷新
func (s *Server) kickCatalogRefresh(force bool) {
	if !force && !s.catalogNeedsRefresh() {
		return
	}
	s.catalogMu.Lock()
	if s.catalogRefreshing {
		s.catalogMu.Unlock()
		return
	}
	s.catalogRefreshing = true
	s.catalogMu.Unlock()

	go s.refreshCatalog()
}

// triggerCatalogRefreshIfStale 搜索无结果时触发：若距上次刷新超过 minStale 则异步刷新
func (s *Server) triggerCatalogRefreshIfStale(minStale time.Duration) {
	s.catalogMu.RLock()
	refreshing := s.catalogRefreshing
	stale := s.catalogAt.IsZero() || time.Since(s.catalogAt) >= minStale
	s.catalogMu.RUnlock()
	if refreshing || !stale {
		return
	}
	s.kickCatalogRefresh(true)
}

func (s *Server) refreshCatalog() {
	defer func() {
		s.catalogMu.Lock()
		s.catalogRefreshing = false
		s.catalogMu.Unlock()
	}()

	if s.dl == nil {
		s.catalogMu.Lock()
		s.catalogLastErr = errNoDownloader.Error()
		s.catalogMu.Unlock()
		return
	}

	root := s.catalogRootURL()
	s.log.Info("refreshing package catalog", zap.String("url", root))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// TTL=0：不把巨型根索引塞进通用包缓存；包名列表自有落盘文件
	body, _, err := s.dl.FetchSimpleIndex(ctx, root, "pypi", 0, true)
	if err != nil {
		s.log.Warn("package catalog refresh failed", zap.Error(err))
		s.catalogMu.Lock()
		s.catalogLastErr = err.Error()
		s.catalogMu.Unlock()
		return
	}

	names := pypihandler.ExtractPackageNames(body)
	sort.Strings(names)
	at := time.Now()

	s.catalogMu.RLock()
	progressive := len(s.catalogNames) == 0
	s.catalogMu.RUnlock()

	// 仅首次（内存尚无数据）分块发布，避免刷新时短暂缩表
	if progressive {
		for i := 0; i < len(names); i += catalogPublishChunk {
			end := i + catalogPublishChunk
			if end > len(names) {
				end = len(names)
			}
			s.publishCatalogChunk(names[:end], root, true)
			if end < len(names) {
				time.Sleep(5 * time.Millisecond)
			}
		}
	}

	if err := s.saveCatalogFile(names, root, at); err != nil {
		s.log.Warn("save package catalog failed", zap.Error(err))
		s.setCatalog(names, root, at, false, err.Error())
		return
	}
	s.setCatalog(names, root, at, false, "")
	s.log.Info("package catalog refreshed",
		zap.Int("names", len(names)),
		zap.String("path", s.catalogPath()),
	)
}

func (s *Server) loadCatalogFile() (names []string, at time.Time, url string, err error) {
	f, err := os.Open(s.catalogPath())
	if err != nil {
		return nil, time.Time{}, "", err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)

	if !sc.Scan() {
		return nil, time.Time{}, "", fmt.Errorf("invalid catalog magic")
	}
	magic := strings.TrimSpace(sc.Text())
	if magic != catalogFileMagic && magic != catalogFileMagicOld {
		return nil, time.Time{}, "", fmt.Errorf("invalid catalog magic")
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if line == "---" {
			break
		}
		if strings.HasPrefix(line, "url=") {
			url = strings.TrimPrefix(line, "url=")
			continue
		}
		if strings.HasPrefix(line, "updated_at=") {
			at, _ = time.Parse(time.RFC3339, strings.TrimPrefix(line, "updated_at="))
			continue
		}
	}
	names = make([]string, 0, 65536)
	for sc.Scan() {
		name := strings.TrimSpace(sc.Text())
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		names = append(names, name)
	}
	if err := sc.Err(); err != nil {
		return nil, time.Time{}, "", err
	}
	if len(names) == 0 {
		return nil, time.Time{}, "", fmt.Errorf("empty catalog file")
	}
	return names, at, url, nil
}

func (s *Server) saveCatalogFile(names []string, url string, at time.Time) error {
	if err := os.MkdirAll(s.catalogDir(), 0o755); err != nil {
		return err
	}
	path := s.catalogPath()
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 256*1024)
	_, _ = fmt.Fprintf(bw, "%s\nurl=%s\nupdated_at=%s\n---\n", catalogFileMagic, url, at.UTC().Format(time.RFC3339))
	for _, name := range names {
		_, _ = bw.WriteString(name)
		_ = bw.WriteByte('\n')
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// ensureCatalogLoaded 只做落盘加载与触发后台刷新，绝不在请求路径拉全量索引
func (s *Server) ensureCatalogLoaded() {
	s.catalogMu.RLock()
	ready := len(s.catalogNames) > 0
	s.catalogMu.RUnlock()
	if !ready {
		if n, at, url, err := s.loadCatalogFile(); err == nil && len(n) > 0 {
			root := s.catalogRootURL()
			if url == "" || url == root {
				s.setCatalog(n, root, at, false, "")
			}
		}
	}
	s.kickCatalogRefresh(false)
}
