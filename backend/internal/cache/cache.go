package cache

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"

	"github.com/livehl/mirrorhub/internal/cachekey"
	"github.com/livehl/mirrorhub/internal/store"
)

var (
	bucketMeta   = []byte("meta")
	bucketChunks = []byte("chunks")
)

type Entry struct {
	Key          string    `json:"key"`
	FilePath     string    `json:"file_path"`
	ContentType  string    `json:"content_type"`
	Size         int64     `json:"size"`
	CreatedAt    time.Time `json:"created_at"`
	LastAccess   time.Time `json:"last_access"`
	TTLSeconds   int       `json:"ttl_seconds"`
	SourceURL    string    `json:"source_url,omitempty"`
	Kind         string    `json:"kind,omitempty"` // index | package | metadata
	Digest       string    `json:"digest,omitempty"`
	ETag         string    `json:"etag,omitempty"`
	UpstreamETag string    `json:"upstream_etag,omitempty"` // 上游响应 ETag，用于条件请求续期
	RepoCommit   string    `json:"repo_commit,omitempty"`   // HF X-Repo-Commit，供 huggingface_hub 识别 Hub 端点
}

// Meta 写入缓存时的附加元数据
type Meta struct {
	SourceURL    string
	Kind         string
	Digest       string
	UpstreamETag string // 上游响应 ETag，用于条件请求续期
	RepoCommit   string // HF X-Repo-Commit
}

type Stats struct {
	Entries          int              `json:"entries"`
	TotalSize        int64            `json:"total_size"`
	MaxSize          int64            `json:"max_size"`
	Hits             int64            `json:"hits"`
	Misses           int64            `json:"misses"`
	HitsByPlatform   map[string]int64 `json:"hits_by_platform,omitempty"`   // 各平台命中次数
	MissesByPlatform map[string]int64 `json:"misses_by_platform,omitempty"` // 各平台未命中次数
	UsageRatio       float64          `json:"usage_ratio"`
	DiskFreeBytes    int64            `json:"disk_free_bytes"`
	DiskFreeOK       bool             `json:"disk_free_ok"`
	WaterWarn        bool             `json:"water_warn"`
	WaterCrit        bool             `json:"water_crit"`
	ByPlatform       map[string]int64 `json:"by_platform,omitempty"` // 各平台已用字节
}

type Manager struct {
	dir     string
	maxSize int64
	db      *bbolt.DB
	mu      sync.Mutex
	hit     hitCounters
	dirty   bool
	store   *store.Store
	stop    chan struct{}
	done    chan struct{}

	// clearing + writeWG：Clear 先置位再等在飞写入结束，避免删光后被并发 Put 写回。
	clearing bool
	writeWG  sync.WaitGroup

	layoutMigrate layoutMigrateStats
}

var errCacheClearing = fmt.Errorf("cache clearing")

// HoldWrite 登记一次在飞写入（下载开始时调用）；Clear 会等待全部 ReleaseWrite。
func (m *Manager) HoldWrite() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.clearing {
		return errCacheClearing
	}
	m.writeWG.Add(1)
	return nil
}

// ReleaseWrite 与 HoldWrite 成对。
func (m *Manager) ReleaseWrite() {
	m.writeWG.Done()
}

type layoutMigrateStats struct {
	moved, skipped, missing, pruned int
}

// ObjectLayoutMigrateStats 返回启动时按平台分区迁移的结果（供日志）。
func (m *Manager) ObjectLayoutMigrateStats() (moved, skipped, missing, pruned int) {
	s := m.layoutMigrate
	return s.moved, s.skipped, s.missing, s.pruned
}

const (
	cacheCounterKey = "stats.cache"
	persistEvery    = 5 * time.Second
)

type cacheCounters struct {
	Hits             int64            `json:"hits"`
	Misses           int64            `json:"misses"`
	HitsByPlatform   map[string]int64 `json:"hits_by_platform,omitempty"`
	MissesByPlatform map[string]int64 `json:"misses_by_platform,omitempty"`
}

func New(dir string, maxSizeGB float64, st *store.Store) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dir, "meta.db")
	db, err := bbolt.Open(dbPath, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		if errors.Is(err, bbolt.ErrTimeout) || strings.Contains(err.Error(), "timeout") {
			return nil, fmt.Errorf("打开缓存库超时（%s 可能被另一个 mirrorhub 进程占用）: %w", dbPath, err)
		}
		return nil, err
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(bucketMeta); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(bucketChunks)
		return err
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if maxSizeGB <= 0 {
		maxSizeGB = 100
	}
	m := &Manager{
		dir:     dir,
		maxSize: int64(maxSizeGB * float64(1024*1024*1024)),
		db:      db,
		store:   st,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	moved, skipped, missing := m.migrateObjectLayout()
	pruned := m.pruneEmptyLegacyObjectDirs()
	m.layoutMigrate = layoutMigrateStats{
		moved: moved, skipped: skipped, missing: missing, pruned: pruned,
	}
	m.loadCounters()
	go m.persistLoop()
	return m, nil
}

// SetMaxSizeGB 热更新容量上限（支持小数 GiB，便于水位测试）；超限时立即触发淘汰。
func (m *Manager) SetMaxSizeGB(gb float64) {
	if gb <= 0 {
		gb = 100
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxSize = int64(gb * float64(1024*1024*1024))
	if m.maxSize < 1 {
		m.maxSize = 1
	}
	_ = m.evictLocked()
}

func (m *Manager) loadCounters() {
	if m.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var c cacheCounters
	ok, err := m.store.GetJSON(ctx, cacheCounterKey, &c)
	if err != nil || !ok {
		return
	}
	m.mu.Lock()
	m.hit.load(c.Hits, c.Misses, c.HitsByPlatform, c.MissesByPlatform)
	m.mu.Unlock()
}

func (m *Manager) saveCounters() {
	if m.store == nil {
		return
	}
	m.mu.Lock()
	if !m.dirty {
		m.mu.Unlock()
		return
	}
	hits, misses, byHit, byMiss := m.hit.snapshot()
	c := cacheCounters{
		Hits:             hits,
		Misses:           misses,
		HitsByPlatform:   byHit,
		MissesByPlatform: byMiss,
	}
	m.dirty = false
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.store.PutJSON(ctx, cacheCounterKey, c)
}

func (m *Manager) persistLoop() {
	defer close(m.done)
	t := time.NewTicker(persistEvery)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			m.mu.Lock()
			m.dirty = true
			m.mu.Unlock()
			m.saveCounters()
			return
		case <-t.C:
			m.saveCounters()
		}
	}
}

func (m *Manager) Close() error {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	<-m.done
	return m.db.Close()
}

func KeyFromURL(url string) string {
	return cachekey.FromURL(url)
}

// sanitizePlatformDir 平台名作目录段；非法字符归 other，避免路径穿越。
func sanitizePlatformDir(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return "other"
	}
	for _, c := range p {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return "other"
	}
	return p
}

func (m *Manager) objectKeySeg(key string) string {
	sk := safeCacheKey(key)
	if len(sk) < 2 {
		sk = sk + "__"
	}
	return sk
}

// filePath 按平台分区：objects/{platform}/{sk[:2]}/{sk}
func (m *Manager) filePath(key, platform string) string {
	sk := m.objectKeySeg(key)
	return filepath.Join(m.dir, "objects", sanitizePlatformDir(platform), sk[:2], sk)
}

// legacyFilePath 旧布局 objects/{sk[:2]}/{sk}（无平台段）。
func (m *Manager) legacyFilePath(key string) string {
	sk := m.objectKeySeg(key)
	return filepath.Join(m.dir, "objects", sk[:2], sk)
}

// migrateObjectLayout 将旧 objects 布局迁到按平台分区，并更新 Entry.FilePath。
func (m *Manager) migrateObjectLayout() (moved, skipped, missing int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var entries []Entry
	_ = m.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		return b.ForEach(func(_, v []byte) error {
			var e Entry
			if json.Unmarshal(v, &e) == nil {
				entries = append(entries, e)
			}
			return nil
		})
	})

	for i := range entries {
		e := &entries[i]
		if e.Key == "" {
			missing++
			continue
		}
		plat := ClassifyPlatform(*e)
		newPath := m.filePath(e.Key, plat)

		if _, err := os.Stat(newPath); err == nil {
			if e.FilePath != newPath {
				e.FilePath = newPath
				_ = m.putMetaLocked(e)
			}
			skipped++
			continue
		}

		src := ""
		if e.FilePath != "" {
			if _, err := os.Stat(e.FilePath); err == nil {
				src = e.FilePath
			}
		}
		if src == "" {
			old := m.legacyFilePath(e.Key)
			if _, err := os.Stat(old); err == nil {
				src = old
			}
		}
		if src == "" {
			missing++
			continue
		}
		if src == newPath {
			if e.FilePath != newPath {
				e.FilePath = newPath
				_ = m.putMetaLocked(e)
			}
			skipped++
			continue
		}
		if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
			missing++
			continue
		}
		if err := moveFile(src, newPath); err != nil {
			missing++
			continue
		}
		e.FilePath = newPath
		_ = m.putMetaLocked(e)
		moved++
	}
	return moved, skipped, missing
}

// isLegacyShardDir 旧布局的两位 hex 分片目录（如 e1），不含平台名。
func isLegacyShardDir(name string) bool {
	if len(name) != 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		c := name[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// pruneEmptyLegacyObjectDirs 删除迁移后留下的空 hex 分片目录，避免顶层看起来仍是旧布局。
func (m *Manager) pruneEmptyLegacyObjectDirs() int {
	objects := filepath.Join(m.dir, "objects")
	ents, err := os.ReadDir(objects)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if !e.IsDir() || !isLegacyShardDir(e.Name()) {
			continue
		}
		path := filepath.Join(objects, e.Name())
		if removeEmptyDirTree(path) {
			n++
		}
	}
	return n
}

// removeEmptyDirTree 若目录树内无文件则整棵删除；有文件则只清空子目录，返回是否删除了根。
func removeEmptyDirTree(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	empty := true
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if !removeEmptyDirTree(p) {
				empty = false
			}
			continue
		}
		empty = false
	}
	if !empty {
		return false
	}
	return os.Remove(dir) == nil
}

func (m *Manager) Get(key string, platformHint ...string) (*Entry, bool) {
	hint := ""
	if len(platformHint) > 0 {
		hint = platformHint[0]
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var entry Entry
	err := m.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		v := b.Get([]byte(key))
		if v == nil {
			return fmt.Errorf("miss")
		}
		return json.Unmarshal(v, &entry)
	})
	if err != nil {
		m.hit.noteMiss(HitPlatform(hint, key, nil))
		m.dirty = true
		return nil, false
	}
	if strings.TrimSpace(entry.SourceURL) == "" || strings.TrimSpace(entry.Kind) == "" {
		_ = m.deleteLocked(key)
		m.hit.noteMiss(HitPlatform(hint, key, &entry))
		m.dirty = true
		return nil, false
	}
	// TTL：index/metadata 按 CreatedAt 过期；package 制品（wheel/sdist）内容不可变，
	// 不按 TTL 失效，仅靠容量淘汰。过期 index 不立即删除，留给 GetStale 条件续期。
	if entry.Kind != "package" && entry.TTLSeconds > 0 && time.Since(entry.CreatedAt) > time.Duration(entry.TTLSeconds)*time.Second {
		m.hit.noteMiss(HitPlatform(hint, key, &entry))
		m.dirty = true
		return nil, false
	}
	info, err := os.Stat(entry.FilePath)
	if err != nil {
		_ = m.deleteLocked(key)
		m.hit.noteMiss(HitPlatform(hint, key, &entry))
		m.dirty = true
		return nil, false
	}
	// 零字节或元数据与磁盘实长不一致：删条回源，避免错误 Content-Length 挂死客户端
	if entry.Kind == "package" && (entry.Size <= 0 || info.Size() != entry.Size) {
		_ = m.deleteLocked(key)
		m.hit.noteMiss(HitPlatform(hint, key, &entry))
		m.dirty = true
		return nil, false
	}
	now := time.Now()
	prevAccess := entry.LastAccess
	entry.LastAccess = now
	// 每次 HIT 同步写 bbolt 会把并发吞吐打成串行；LastAccess 最多约 30s 落盘一次（淘汰仍可用）
	if prevAccess.IsZero() || now.Sub(prevAccess) >= 30*time.Second {
		_ = m.putMetaLocked(&entry)
	}
	m.hit.noteHit(HitPlatform(hint, key, &entry))
	m.dirty = true
	return &entry, true
}

// Open 持锁校验后打开文件，降低 Get 与 Put/Delete 之间的 TOCTOU。
// 调用方必须 Close 返回的 *os.File。
func (m *Manager) Open(key string, platformHint ...string) (*Entry, *os.File, bool) {
	hint := ""
	if len(platformHint) > 0 {
		hint = platformHint[0]
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var entry Entry
	err := m.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		v := b.Get([]byte(key))
		if v == nil {
			return fmt.Errorf("miss")
		}
		return json.Unmarshal(v, &entry)
	})
	if err != nil {
		m.hit.noteMiss(HitPlatform(hint, key, nil))
		m.dirty = true
		return nil, nil, false
	}
	if strings.TrimSpace(entry.SourceURL) == "" || strings.TrimSpace(entry.Kind) == "" {
		_ = m.deleteLocked(key)
		m.hit.noteMiss(HitPlatform(hint, key, &entry))
		m.dirty = true
		return nil, nil, false
	}
	if entry.Kind != "package" && entry.TTLSeconds > 0 && time.Since(entry.CreatedAt) > time.Duration(entry.TTLSeconds)*time.Second {
		m.hit.noteMiss(HitPlatform(hint, key, &entry))
		m.dirty = true
		return nil, nil, false
	}
	info, err := os.Stat(entry.FilePath)
	if err != nil {
		_ = m.deleteLocked(key)
		m.hit.noteMiss(HitPlatform(hint, key, &entry))
		m.dirty = true
		return nil, nil, false
	}
	if entry.Kind == "package" && (entry.Size <= 0 || info.Size() != entry.Size) {
		_ = m.deleteLocked(key)
		m.hit.noteMiss(HitPlatform(hint, key, &entry))
		m.dirty = true
		return nil, nil, false
	}
	f, err := os.Open(entry.FilePath)
	if err != nil {
		_ = m.deleteLocked(key)
		m.hit.noteMiss(HitPlatform(hint, key, &entry))
		m.dirty = true
		return nil, nil, false
	}
	now := time.Now()
	prevAccess := entry.LastAccess
	entry.LastAccess = now
	if prevAccess.IsZero() || now.Sub(prevAccess) >= 30*time.Second {
		_ = m.putMetaLocked(&entry)
	}
	m.hit.noteHit(HitPlatform(hint, key, &entry))
	m.dirty = true
	cp := entry
	return &cp, f, true
}

// GetStale 返回过期但物理文件仍存在的条目，不检查 TTL、不删除、不更新 LastAccess。
// 用于 revalidation 场景：拿到旧 entry 的 UpstreamETag 向上游发条件请求。
func (m *Manager) GetStale(key string) (*Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var entry Entry
	err := m.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		v := b.Get([]byte(key))
		if v == nil {
			return fmt.Errorf("miss")
		}
		return json.Unmarshal(v, &entry)
	})
	if err != nil {
		return nil, false
	}
	if strings.TrimSpace(entry.SourceURL) == "" || strings.TrimSpace(entry.Kind) == "" {
		return nil, false
	}
	if _, err := os.Stat(entry.FilePath); err != nil {
		return nil, false
	}
	return &entry, true
}

// OpenStale 与 GetStale 同等语义（忽略 TTL），但在持锁下打开文件，降低 TOCTOU。
// 调用方必须 Close 返回的 *os.File。不计命中统计。
func (m *Manager) OpenStale(key string) (*Entry, *os.File, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var entry Entry
	err := m.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		v := b.Get([]byte(key))
		if v == nil {
			return fmt.Errorf("miss")
		}
		return json.Unmarshal(v, &entry)
	})
	if err != nil {
		return nil, nil, false
	}
	if strings.TrimSpace(entry.SourceURL) == "" || strings.TrimSpace(entry.Kind) == "" {
		return nil, nil, false
	}
	f, err := os.Open(entry.FilePath)
	if err != nil {
		return nil, nil, false
	}
	cp := entry
	return &cp, f, true
}

// ReadAll 持锁 Open 后读完整文件（适合索引/元数据等小对象），避免 Get+ReadFile TOCTOU。
func (m *Manager) ReadAll(key string, platformHint ...string) (*Entry, []byte, bool) {
	ent, f, ok := m.Open(key, platformHint...)
	if !ok {
		return nil, nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, false
	}
	return ent, data, true
}

// ReadAllStale 持锁 OpenStale 后读完整文件。
func (m *Manager) ReadAllStale(key string) (*Entry, []byte, bool) {
	ent, f, ok := m.OpenStale(key)
	if !ok {
		return nil, nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, false
	}
	return ent, data, true
}

// RenewTTL 更新条目的 CreatedAt、TTLSeconds 和 LastAccess（不改文件），实现条件请求续期。
func (m *Manager) RenewTTL(key string, ttlSeconds int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var entry Entry
	err := m.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		v := b.Get([]byte(key))
		if v == nil {
			return fmt.Errorf("miss")
		}
		return json.Unmarshal(v, &entry)
	})
	if err != nil {
		return err
	}
	entry.CreatedAt = time.Now()
	entry.LastAccess = time.Now()
	entry.TTLSeconds = ttlSeconds
	return m.putMetaLocked(&entry)
}

func (m *Manager) Put(key, srcPath, contentType string, ttlSeconds int, meta Meta) (*Entry, error) {
	if strings.TrimSpace(meta.SourceURL) == "" || strings.TrimSpace(meta.Kind) == "" {
		return nil, fmt.Errorf("cache meta requires source_url and kind")
	}
	// 自身占写租约（下载路径还会再 Hold 一层，Clear 等两层都结束）
	if err := m.HoldWrite(); err != nil {
		_ = os.Remove(srcPath)
		return nil, err
	}
	defer m.ReleaseWrite()

	// 入库摘要统一为 sha256 hex；npm SRI（sha512-/sha256-）先校验再换算，避免 Put 把 SRI 当 hex 比对失败。
	digestHex := ""
	if strings.TrimSpace(meta.Digest) != "" {
		sum, err := verifyAndNormalizeDigest(srcPath, meta.Digest)
		if err != nil {
			_ = os.Remove(srcPath)
			return nil, err
		}
		digestHex = sum
	}

	plat := ClassifyPlatform(Entry{Key: key, SourceURL: meta.SourceURL, Kind: meta.Kind})
	dst := m.filePath(key, plat)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	// 锁外完成大文件 move：先落到旁路临时名再 rename 覆盖，缩短持锁时间
	staging := dst + ".put-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := moveFile(srcPath, staging); err != nil {
		return nil, err
	}
	info, err := os.Stat(staging)
	if err != nil {
		_ = os.Remove(staging)
		return nil, err
	}
	kind := strings.TrimSpace(meta.Kind)
	if kind == "" {
		kind = "package"
	}
	if kind == "package" && info.Size() <= 0 {
		_ = os.Remove(staging)
		return nil, fmt.Errorf("refuse to cache zero-size package")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.clearing {
		_ = os.Remove(staging)
		return nil, errCacheClearing
	}

	_ = os.Remove(dst)
	if err := os.Rename(staging, dst); err != nil {
		// rename 失败则拷贝兜底
		if copyErr := moveFile(staging, dst); copyErr != nil {
			_ = os.Remove(staging)
			return nil, copyErr
		}
	}
	info, err = os.Stat(dst)
	if err != nil {
		return nil, err
	}
	etag := ""
	if digestHex != "" {
		etag = `"` + digestHex + `"`
	} else {
		etag = fmt.Sprintf(`W/"%d-%d"`, info.Size(), time.Now().Unix())
	}
	entry := &Entry{
		Key:          key,
		FilePath:     dst,
		ContentType:  contentType,
		Size:         info.Size(),
		CreatedAt:    time.Now(),
		LastAccess:   time.Now(),
		TTLSeconds:   ttlSeconds,
		SourceURL:    meta.SourceURL,
		Kind:         kind,
		Digest:       digestHex,
		ETag:         etag,
		UpstreamETag: meta.UpstreamETag,
		RepoCommit:   meta.RepoCommit,
	}
	if err := m.putMetaLocked(entry); err != nil {
		return nil, err
	}
	_ = m.evictLocked()
	return entry, nil
}

// moveFile 优先 rename，跨设备失败则流式拷贝（避免整文件读入内存）
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return closeErr
	}
	_ = os.Remove(src)
	return nil
}

func (m *Manager) PutBytes(key string, body []byte, contentType string, ttlSeconds int, meta Meta) (*Entry, error) {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return nil, err
	}
	tmp := filepath.Join(m.dir, "tmp_"+safeCacheKey(key))
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	return m.Put(key, tmp, contentType, ttlSeconds, meta)
}

// safeCacheKey 将任意 cache key 映射为固定 hex 路径段（与宿主机文件系统无关）。
func safeCacheKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

func (m *Manager) TempPath(key string) string {
	return filepath.Join(m.dir, "tmp_"+safeCacheKey(key)+".part")
}

func (m *Manager) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteLocked(key)
}

// Clear 清空缓存。abortInflight 在 clearing=true 之后、Wait 之前调用（用于取消已 Bind 的下载），
// 避免「先取消再置 clearing」窗口内新 HoldWrite 拖住 Wait。
func (m *Manager) Clear(abortInflight func()) error {
	m.mu.Lock()
	m.clearing = true
	m.mu.Unlock()

	if abortInflight != nil {
		abortInflight()
	}
	// 在飞写入会因 ctx 取消尽快 ReleaseWrite；新 HoldWrite 已因 clearing 失败
	m.writeWG.Wait()

	m.mu.Lock()
	var keys []string
	_ = m.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		return b.ForEach(func(k, _ []byte) error {
			keys = append(keys, string(k))
			return nil
		})
	})
	for _, k := range keys {
		_ = m.deleteLocked(k)
	}
	// 清空分片续传标记，避免「条目已删但 chunks 仍在」导致错误 resume
	_ = m.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.DeleteBucket(bucketChunks); err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(bucketChunks)
		return err
	})
	m.hit.clear()
	m.dirty = true
	m.mu.Unlock()

	// clearing 仍为 true：禁止新 HoldWrite，期间清临时文件
	_, _ = m.PurgeTempFiles()

	m.mu.Lock()
	m.clearing = false
	m.mu.Unlock()
	return nil
}

func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total int64
	var n int
	byPlat := map[string]int64{}
	_ = m.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		return b.ForEach(func(_, v []byte) error {
			var e Entry
			if json.Unmarshal(v, &e) == nil {
				total += e.Size
				n++
				p := ClassifyPlatform(e)
				byPlat[p] += e.Size
			}
			return nil
		})
	})
	ratio := 0.0
	if m.maxSize > 0 {
		ratio = float64(total) / float64(m.maxSize)
	}
	hits, misses, byHit, byMiss := m.hit.snapshot()
	free, ok := diskFreeBytes(m.dir)
	return Stats{
		Entries:          n,
		TotalSize:        total,
		MaxSize:          m.maxSize,
		Hits:             hits,
		Misses:           misses,
		HitsByPlatform:   byHit,
		MissesByPlatform: byMiss,
		UsageRatio:       ratio,
		DiskFreeBytes:    free,
		DiskFreeOK:       ok,
		WaterWarn:        ratio >= 0.80,
		WaterCrit:        ratio >= 0.92,
		ByPlatform:       byPlat,
	}
}

// AllowPrefetchWrite 磁盘/容量临界时拒绝预取写入
func (m *Manager) AllowPrefetchWrite() error {
	st := m.Stats()
	if st.WaterCrit {
		return fmt.Errorf("cache water critical (usage=%.1f%%)", st.UsageRatio*100)
	}
	if st.DiskFreeOK && st.DiskFreeBytes < 1<<30 {
		return fmt.Errorf("disk free too low: %d bytes", st.DiskFreeBytes)
	}
	return nil
}

// List 返回全部缓存条目（按最近访问倒序）
func (m *Manager) List() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Entry
	_ = m.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		return b.ForEach(func(_, v []byte) error {
			var e Entry
			if json.Unmarshal(v, &e) == nil {
				out = append(out, e)
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastAccess.After(out[j].LastAccess)
	})
	return out
}

// PurgeIncomplete 删除缺少 SourceURL 或 Kind 的旧缓存条目及其文件。
func (m *Manager) PurgeIncomplete() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var keys []string
	_ = m.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		return b.ForEach(func(k, v []byte) error {
			var e Entry
			if json.Unmarshal(v, &e) != nil {
				keys = append(keys, string(k))
				return nil
			}
			if strings.TrimSpace(e.SourceURL) == "" || strings.TrimSpace(e.Kind) == "" {
				keys = append(keys, e.Key)
				if e.Key == "" {
					keys[len(keys)-1] = string(k)
				}
			}
			return nil
		})
	})
	for _, key := range keys {
		if err := m.deleteLocked(key); err != nil {
			return 0, err
		}
	}
	return len(keys), nil
}

// PurgeTempFiles 清理缓存目录下孤儿临时文件（tmp_*）
func (m *Manager) PurgeTempFiles() (int, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "tmp_") {
			continue
		}
		if err := os.Remove(filepath.Join(m.dir, name)); err == nil {
			n++
		}
	}
	return n, nil
}

type ChunkMark struct {
	URL       string    `json:"url"`
	TotalSize int64     `json:"total_size"`
	Start     int64     `json:"start"`
	End       int64     `json:"end"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (m *Manager) MarkChunks(url string, totalSize int64, ranges [][2]int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketChunks)
		for _, r := range ranges {
			mark := ChunkMark{
				URL: url, TotalSize: totalSize, Start: r[0], End: r[1], UpdatedAt: time.Now(),
			}
			key := chunkKey(url, r[0], r[1])
			data, _ := json.Marshal(mark)
			if err := b.Put([]byte(key), data); err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *Manager) GetChunks(url string, totalSize int64, ttlHours int) [][2]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out [][2]int64
	cutoff := time.Now().Add(-time.Duration(ttlHours) * time.Hour)
	_ = m.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketChunks)
		return b.ForEach(func(_, v []byte) error {
			var mark ChunkMark
			if json.Unmarshal(v, &mark) != nil {
				return nil
			}
			if mark.URL != url || mark.TotalSize != totalSize {
				return nil
			}
			if mark.UpdatedAt.Before(cutoff) {
				return nil
			}
			out = append(out, [2]int64{mark.Start, mark.End})
			return nil
		})
	})
	return out
}

func (m *Manager) ClearChunks(url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketChunks)
		var keys [][]byte
		_ = b.ForEach(func(k, v []byte) error {
			var mark ChunkMark
			if json.Unmarshal(v, &mark) == nil && mark.URL == url {
				keys = append(keys, append([]byte{}, k...))
			}
			return nil
		})
		for _, k := range keys {
			_ = b.Delete(k)
		}
		return nil
	})
}

func chunkKey(url string, start, end int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", url, start, end)))
	return hex.EncodeToString(sum[:16])
}

func (m *Manager) putMetaLocked(entry *Entry) error {
	return m.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		data, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		return b.Put([]byte(entry.Key), data)
	})
}

func (m *Manager) deleteLocked(key string) error {
	var entry Entry
	_ = m.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		v := b.Get([]byte(key))
		if v != nil {
			_ = json.Unmarshal(v, &entry)
		}
		return nil
	})
	if entry.FilePath != "" {
		_ = os.Remove(entry.FilePath)
	}
	return m.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Delete([]byte(key))
	})
}

func (m *Manager) evictLocked() error {
	type item struct {
		key  string
		e    Entry
	}
	var items []item
	var total int64
	_ = m.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		return b.ForEach(func(k, v []byte) error {
			var e Entry
			if json.Unmarshal(v, &e) == nil {
				items = append(items, item{key: string(k), e: e})
				total += e.Size
			}
			return nil
		})
	})
	if total <= m.maxSize {
		return nil
	}
	target := m.maxSize
	// 超过 80% 时多腾出一些空间
	if float64(total) >= float64(m.maxSize)*0.80 {
		target = int64(float64(m.maxSize) * 0.75)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].e.LastAccess.Before(items[j].e.LastAccess)
	})
	for _, it := range items {
		if total <= target {
			break
		}
		_ = m.deleteLocked(it.key)
		total -= it.e.Size
	}
	return nil
}

func fileSHA256Hex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileDigestBase64(path string, h hash.Hash) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// verifyAndNormalizeDigest 校验期望摘要（裸 sha256 hex / SRI），成功后返回用于入库的小写 sha256 hex。
func verifyAndNormalizeDigest(path, expected string) (string, error) {
	exp := strings.TrimSpace(expected)
	if exp == "" {
		return "", nil
	}
	sumHex, err := fileSHA256Hex(path)
	if err != nil {
		return "", err
	}
	switch {
	case strings.HasPrefix(exp, "sha512-"):
		sum, err := fileDigestBase64(path, sha512.New())
		if err != nil {
			return "", err
		}
		want := strings.TrimPrefix(exp, "sha512-")
		if sum != want {
			return "", fmt.Errorf("sha512 mismatch: want %s got %s", want, sum)
		}
		return sumHex, nil
	case strings.HasPrefix(exp, "sha256-"):
		sum, err := fileDigestBase64(path, sha256.New())
		if err != nil {
			return "", err
		}
		want := strings.TrimPrefix(exp, "sha256-")
		if sum != want {
			return "", fmt.Errorf("sha256 mismatch: want %s got %s", want, sum)
		}
		return sumHex, nil
	default:
		if !strings.EqualFold(sumHex, exp) {
			return "", fmt.Errorf("sha256 mismatch: want %s got %s", exp, sumHex)
		}
		return strings.ToLower(sumHex), nil
	}
}

