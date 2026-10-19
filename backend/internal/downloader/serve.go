package downloader

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/nihaoyanzu/cistern/internal/cache"
)

// ServePackage 交互下载：命中缓存直接回文件；未命中则流式并写入缓存。
func (e *Engine) ServePackage(ctx context.Context, w http.ResponseWriter, opt Options) (cacheLabel string, err error) {
	opt.resolveExpectedDigest()
	if entry, f, ok := e.cache.Open(opt.CacheKey, opt.Platform); ok {
		notifyHit(opt)
		defer f.Close()
		// ctx 上已有 Hub commit 而条目缺头：补写后再回客户端（HF HIT 兼容）
		if meta := HubFileMetaFrom(ctx); meta != nil && entry != nil {
			if c := meta.GetRepoCommit(); c != "" && strings.TrimSpace(entry.RepoCommit) == "" {
				if err := e.cache.SetRepoCommit(entry.Key, c); err == nil {
					entry.RepoCommit = c
				}
			}
		}
		return serveCachedFile(w, entry, f, opt.Boost, opt.RangeHeader, "HIT")
	}

	for try := 0; try < 3; try++ {
		label, done, err := e.servePackageOnce(ctx, w, opt)
		if done {
			return label, err
		}
	}
	return "miss", fmt.Errorf("unable to acquire download slot")
}

// ServeCachedFile 使用调用方已通过 cache.Open/OpenStale 持有的 fd。
func (e *Engine) ServeCachedFile(w http.ResponseWriter, entry *cache.Entry, f *os.File, boost bool, rangeHeader, cacheLabel string) (string, error) {
	return serveCachedFile(w, entry, f, boost, rangeHeader, cacheLabel)
}

// ServeCachedEntry 按 key 持锁再开文件后回源，避免 Get/GetStale 与 Open 之间的 TOCTOU。
// cacheLabel 为 STALE 时走 OpenStale（忽略 TTL）；否则 Open。空则按 HIT。
func (e *Engine) ServeCachedEntry(w http.ResponseWriter, entry *cache.Entry, boost bool, rangeHeader, cacheLabel string) (string, error) {
	if entry == nil || strings.TrimSpace(entry.Key) == "" {
		return "miss", fmt.Errorf("nil cache entry")
	}
	label := cacheLabel
	if label == "" {
		label = "HIT"
	}
	var (
		ent *cache.Entry
		f   *os.File
		ok  bool
	)
	if strings.EqualFold(label, "STALE") {
		ent, f, ok = e.cache.OpenStale(entry.Key)
	} else {
		ent, f, ok = e.cache.Open(entry.Key)
	}
	if !ok {
		return "miss", fmt.Errorf("cache open miss")
	}
	defer f.Close()
	return serveCachedFile(w, ent, f, boost, rangeHeader, label)
}

func (e *Engine) servePackageOnce(ctx context.Context, w http.ResponseWriter, opt Options) (label string, done bool, err error) {
	wait := &inflightWait{done: make(chan struct{}), mode: inflightModeStream}
	actual, loaded := e.inflight.LoadOrStore(opt.CacheKey, wait)
	if loaded {
		sw := actual.(*inflightWait)
		if sw.mode == inflightModeFetch && sw.cancel != nil {
			sw.cancel()
		}
		if opt.OnWaiting != nil {
			opt.OnWaiting()
		}
		select {
		case <-ctx.Done():
			return "na", true, ctx.Err()
		case <-sw.done:
		}
		if entry, f, ok := e.cache.Open(opt.CacheKey, opt.Platform); ok {
			notifyHit(opt)
			defer f.Close()
			if meta := HubFileMetaFrom(ctx); meta != nil && entry != nil {
				if c := meta.GetRepoCommit(); c != "" && strings.TrimSpace(entry.RepoCommit) == "" {
					if err := e.cache.SetRepoCommit(entry.Key, c); err == nil {
						entry.RepoCommit = c
					}
				}
			}
			l, e2 := serveCachedFile(w, entry, f, opt.Boost, opt.RangeHeader, "HIT")
			return l, true, e2
		}
		if sw.err == nil && sw.entry != nil && strings.TrimSpace(sw.entry.Key) != "" {
			notifyHit(opt)
			l, e2 := e.ServeCachedEntry(w, sw.entry, opt.Boost, opt.RangeHeader, "HIT")
			return l, true, e2
		}
		return "", false, nil // 重试占坑
	}

	defer func() {
		e.inflight.Delete(opt.CacheKey)
		close(wait.done)
	}()

	var entry *cache.Entry
	// 交互式下载始终使用串行流式（streamToClientAndCache）：
	// - 有卡顿检测（streamStallTimeout=10s）和自动 Range 续传
	// - 单连接下载，不受并发分片带宽分配影响
	// - 对慢上游镜像更可靠，避免 pip ReadTimeout
	// 预取下载使用 GetOrDownload → download（并行分片），不经过此路径。
	entry, _, _, err = e.streamToClientAndCache(ctx, w, opt)
	wait.err = err
	wait.entry = entry
	if entry != nil {
		wait.ct = entry.ContentType
		wait.size = entry.Size
	}
	if err != nil {
		return "miss", true, err
	}
	return "miss", true, nil
}

func serveCachedFile(w http.ResponseWriter, entry *cache.Entry, f *os.File, boost bool, rangeHeader, cacheLabel string) (string, error) {
	if cacheLabel == "" {
		cacheLabel = "HIT"
	}
	accessLabel := strings.ToLower(cacheLabel)
	if accessLabel == "" {
		accessLabel = "hit"
	}
	ct := entry.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	etag := entry.ETag
	if etag == "" && entry.Digest != "" {
		etag = `"` + entry.Digest + `"`
	}
	if etag == "" {
		etag = fmt.Sprintf(`W/"%d-%d"`, entry.Size, entry.CreatedAt.Unix())
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Cache", cacheLabel)
	w.Header().Set("X-Cistern-Strategy", "cache")
	if boost {
		w.Header().Set("X-Cistern-Boost", "1")
	}
	if entry.RepoCommit != "" {
		w.Header().Set("X-Repo-Commit", entry.RepoCommit)
	}

	start, end, ok, err := parseSingleRange(rangeHeader, entry.Size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", entry.Size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		// 已向客户端完成 416，不记为任务失败
		return accessLabel, nil
	}
	if ok {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return accessLabel, err
		}
		length := end - start + 1
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, entry.Size))
		w.WriteHeader(http.StatusPartialContent)
		_, err = copyFlushN(w, f, length)
		return accessLabel, err
	}

	w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	_, err = copyFlush(w, f)
	return accessLabel, err
}

// parseSingleRange 解析单个 bytes=start-end；多区间返回 error→416；无 Range 返回 ok=false。
func parseSingleRange(header string, size int64) (start, end int64, ok bool, err error) {
	header = strings.TrimSpace(header)
	if header == "" || size <= 0 {
		return 0, 0, false, nil
	}
	if !strings.HasPrefix(strings.ToLower(header), "bytes=") {
		return 0, 0, false, fmt.Errorf("unsupported range unit")
	}
	spec := strings.TrimSpace(header[len("bytes="):])
	if strings.Contains(spec, ",") {
		return 0, 0, false, fmt.Errorf("multiple ranges not supported")
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false, fmt.Errorf("invalid range")
	}
	if parts[0] == "" {
		// suffix: bytes=-N
		n, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil || n <= 0 {
			return 0, 0, false, fmt.Errorf("invalid suffix range")
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, nil
	}
	start, e1 := strconv.ParseInt(parts[0], 10, 64)
	if e1 != nil || start < 0 {
		return 0, 0, false, fmt.Errorf("invalid range start")
	}
	if parts[1] == "" {
		end = size - 1
	} else {
		end, e1 = strconv.ParseInt(parts[1], 10, 64)
		if e1 != nil || end < start {
			return 0, 0, false, fmt.Errorf("invalid range end")
		}
	}
	if start >= size {
		return 0, 0, false, fmt.Errorf("range not satisfiable")
	}
	if end >= size {
		end = size - 1
	}
	return start, end, true, nil
}

func copyFlushN(w http.ResponseWriter, src io.Reader, n int64) (int64, error) {
	return copyFlush(w, io.LimitReader(src, n))
}

func copyFlush(w http.ResponseWriter, src io.Reader) (int64, error) {
	buf := make([]byte, 256*1024)
	var written int64
	var since int
	flusher, canFlush := w.(http.Flusher)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return written, err
			}
			written += int64(n)
			since += n
			if canFlush && since >= streamFlushEvery {
				flusher.Flush()
				since = 0
			}
		}
		if readErr == io.EOF {
			flushWriter(w)
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}
