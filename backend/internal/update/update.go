// Package update 通过 GitHub Releases 检测版本并支持在线替换二进制。
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/livehl/mirrorhub/internal/config"
	"github.com/livehl/mirrorhub/internal/platform"
	"github.com/livehl/mirrorhub/internal/reexec"
	"github.com/livehl/mirrorhub/internal/version"
)

const (
	githubOwner = "nihaoyanzu"
	githubRepo  = "mirrorhub"
	assetPrefix = "mirrorhub_"
	checkTTL    = 10 * time.Minute
	apiTimeout  = 45 * time.Second
)

// 进度阶段
const (
	PhaseIdle        = "idle"
	PhasePreparing   = "preparing"
	PhaseDownloading = "downloading"
	PhaseVerifying   = "verifying"
	PhaseReplacing   = "replacing"
	PhaseRestarting  = "restarting"
	PhaseFailed      = "failed"
	PhaseCancelled   = "cancelled"
)

// Status 版本检测结果。
type Status struct {
	Current         string    `json:"current"`
	Latest          string    `json:"latest,omitempty"`
	UpdateAvailable bool      `json:"update_available"`
	ReleaseURL      string    `json:"release_url,omitempty"`
	AssetName       string    `json:"asset_name,omitempty"`
	Error           string    `json:"error,omitempty"`
	CheckedAt       time.Time `json:"checked_at"`
	Progress        Progress  `json:"progress"`
}

// Progress 在线升级进度（可轮询）。
type Progress struct {
	Phase      string  `json:"phase"`
	BytesDone  int64   `json:"bytes_done"`
	BytesTotal int64   `json:"bytes_total"`
	Percent    float64 `json:"percent"`
	Message    string  `json:"message,omitempty"`
	Error      string  `json:"error,omitempty"`
	CanCancel  bool    `json:"can_cancel"`
}

// Checker 带短缓存的 GitHub 版本检测与升级。
type Checker struct {
	cfg *config.Manager
	log *zap.Logger

	mu      sync.Mutex
	cached  *Status
	cacheAt time.Time

	applyMu sync.Mutex

	progMu sync.RWMutex
	prog   Progress

	cancelMu    sync.Mutex
	applyCancel context.CancelFunc

	// BeforeApply 在下载前调用（如清空调度/预取队列）；可选。
	BeforeApply func()
}

func New(cfg *config.Manager, log *zap.Logger) *Checker {
	return &Checker{
		cfg: cfg,
		log: log,
		prog: Progress{Phase: PhaseIdle},
	}
}

func phaseCanCancel(phase string) bool {
	switch phase {
	case PhasePreparing, PhaseDownloading, PhaseVerifying:
		return true
	default:
		return false
	}
}

func (c *Checker) setProgress(phase string, done, total int64, msg, errMsg string) {
	var pct float64
	if total > 0 {
		pct = float64(done) / float64(total) * 100
		if pct > 100 {
			pct = 100
		}
	} else if phase == PhaseRestarting || phase == PhaseReplacing || phase == PhaseVerifying {
		pct = 100
	}
	c.progMu.Lock()
	c.prog = Progress{
		Phase:      phase,
		BytesDone:  done,
		BytesTotal: total,
		Percent:    pct,
		Message:    msg,
		Error:      errMsg,
		CanCancel:  phaseCanCancel(phase),
	}
	c.progMu.Unlock()
}

// Cancel 取消进行中的升级（准备/下载/校验阶段）；替换与重启阶段不可取消。
func (c *Checker) Cancel() error {
	prog := c.SnapshotProgress()
	if !phaseCanCancel(prog.Phase) {
		if prog.Phase == PhaseReplacing || prog.Phase == PhaseRestarting {
			return errors.New("已进入替换/重启阶段，无法取消")
		}
		return errors.New("当前没有可取消的升级")
	}
	c.cancelMu.Lock()
	cancel := c.applyCancel
	c.cancelMu.Unlock()
	if cancel == nil {
		return errors.New("当前没有可取消的升级")
	}
	cancel()
	c.setProgress(PhaseCancelled, prog.BytesDone, prog.BytesTotal, "正在取消…", "")
	return nil
}

// SnapshotProgress 返回当前升级进度快照。
func (c *Checker) SnapshotProgress() Progress {
	c.progMu.RLock()
	defer c.progMu.RUnlock()
	return c.prog
}

func (c *Checker) apiClient() *http.Client {
	proxy := ""
	if c.cfg != nil {
		proxy = c.cfg.Get().Server.UpstreamProxy
	}
	return platform.NewOutboundClient(proxy, apiTimeout)
}

// downloadClient 大文件下载：整体超时交给 context，避免 Client.Timeout 截断。
func (c *Checker) downloadClient() *http.Client {
	proxy := ""
	if c.cfg != nil {
		proxy = c.cfg.Get().Server.UpstreamProxy
	}
	cl := platform.NewOutboundClient(proxy, 60*time.Second)
	cl.Timeout = 0 // 由 Apply 的 context 控制总时长
	return cl
}

// Status 返回当前版本检测结果；force 为 true 时跳过缓存。
func (c *Checker) Status(ctx context.Context, force bool) Status {
	c.mu.Lock()
	if !force && c.cached != nil && time.Since(c.cacheAt) < checkTTL {
		st := *c.cached
		c.mu.Unlock()
		st.Progress = c.SnapshotProgress()
		return st
	}
	c.mu.Unlock()

	st := c.fetch(ctx)
	c.mu.Lock()
	c.cached = &st
	c.cacheAt = time.Now()
	c.mu.Unlock()
	st.Progress = c.SnapshotProgress()
	return st
}

func (c *Checker) fetch(ctx context.Context) Status {
	st := Status{
		Current:   normalizeVersion(version.Version),
		CheckedAt: time.Now().UTC(),
	}
	rel, err := c.fetchLatest(ctx)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Latest = normalizeVersion(rel.TagName)
	st.ReleaseURL = rel.HTMLURL
	asset := pickAsset(rel.Assets)
	if asset != nil {
		st.AssetName = asset.Name
	}
	if st.Latest != "" && Compare(st.Latest, st.Current) > 0 {
		st.UpdateAvailable = true
	}
	return st
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	HTMLURL string    `json:"html_url"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

func (c *Checker) fetchLatest(ctx context.Context) (*ghRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", githubOwner, githubRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "mirrorhub/"+version.Version)

	resp, err := c.apiClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法访问 GitHub: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("解析 Release 失败: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, errors.New("Release 无 tag_name")
	}
	return &rel, nil
}

func pickAsset(assets []ghAsset) *ghAsset {
	want := assetPrefix + runtime.GOOS + "_" + runtime.GOARCH
	for i := range assets {
		if assets[i].Name == want {
			return &assets[i]
		}
	}
	return nil
}

func pickChecksums(assets []ghAsset) *ghAsset {
	for i := range assets {
		n := strings.ToUpper(assets[i].Name)
		if n == "SHA256SUMS" || n == "CHECKSUMS" || strings.HasSuffix(n, ".SHA256SUMS") {
			return &assets[i]
		}
	}
	return nil
}

func wrapCancel(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	// http 客户端在 context 取消时常见包装错误
	if strings.Contains(err.Error(), "context canceled") {
		return context.Canceled
	}
	return err
}

// Apply 下载最新资产、校验、替换当前可执行文件并 Exec 重启。成功 Exec 时本函数不返回。
func (c *Checker) Apply(ctx context.Context) (err error) {
	if !c.applyMu.TryLock() {
		return errors.New("升级正在进行中")
	}
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			c.applyMu.Unlock()
		}
	}
	defer unlock()

	applyCtx, cancel := context.WithCancel(ctx)
	c.cancelMu.Lock()
	c.applyCancel = cancel
	c.cancelMu.Unlock()
	defer func() {
		cancel()
		c.cancelMu.Lock()
		c.applyCancel = nil
		c.cancelMu.Unlock()
	}()

	var tmpPath string
	defer func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
		if err == nil {
			return
		}
		err = wrapCancel(err)
		if errors.Is(err, context.Canceled) {
			prog := c.SnapshotProgress()
			c.setProgress(PhaseCancelled, prog.BytesDone, prog.BytesTotal, "已取消升级", "")
			return
		}
		if c.SnapshotProgress().Phase != PhaseCancelled {
			c.setProgress(PhaseFailed, 0, 0, "", err.Error())
		}
	}()

	c.setProgress(PhasePreparing, 0, 0, "准备升级（将清空预拉取队列）", "")
	if c.BeforeApply != nil {
		c.BeforeApply()
	}
	if err := applyCtx.Err(); err != nil {
		return err
	}

	rel, err := c.fetchLatest(applyCtx)
	if err != nil {
		return err
	}
	latest := normalizeVersion(rel.TagName)
	current := normalizeVersion(version.Version)
	if Compare(latest, current) <= 0 {
		return fmt.Errorf("已是最新版本 %s", current)
	}
	asset := pickAsset(rel.Assets)
	if asset == nil {
		return fmt.Errorf("Release 无匹配资产 %s%s_%s", assetPrefix, runtime.GOOS, runtime.GOARCH)
	}

	var expectSHA string
	if sumAsset := pickChecksums(rel.Assets); sumAsset != nil {
		sums, sumErr := c.downloadBytes(applyCtx, sumAsset.BrowserDownloadURL, 1<<20)
		if sumErr != nil {
			if wrapCancel(sumErr) == context.Canceled || errors.Is(sumErr, context.Canceled) {
				return context.Canceled
			}
			c.log.Warn("下载 SHA256SUMS 失败，跳过校验", zap.Error(sumErr))
		} else {
			expectSHA = parseSHA256Sums(string(sums), asset.Name)
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位可执行文件失败: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("解析可执行路径失败: %w", err)
	}

	tmpPath = exe + ".new"
	_ = os.Remove(tmpPath)
	c.setProgress(PhaseDownloading, 0, asset.Size, "正在下载 "+asset.Name, "")
	if err := c.downloadFile(applyCtx, asset.BrowserDownloadURL, tmpPath, asset.Size); err != nil {
		return wrapCancel(err)
	}
	if expectSHA != "" {
		c.setProgress(PhaseVerifying, asset.Size, asset.Size, "校验 SHA256", "")
		if err := applyCtx.Err(); err != nil {
			return err
		}
		got, hashErr := fileSHA256(tmpPath)
		if hashErr != nil {
			return hashErr
		}
		if !strings.EqualFold(got, expectSHA) {
			return fmt.Errorf("SHA256 不匹配: expect %s got %s", expectSHA, got)
		}
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	if err := applyCtx.Err(); err != nil {
		return err
	}

	// 进入替换阶段后不可取消
	c.setProgress(PhaseReplacing, asset.Size, asset.Size, "替换二进制", "")
	backup := exe + ".bak"
	_ = os.Remove(backup)
	if err := os.Rename(exe, backup); err != nil {
		if err2 := os.Rename(tmpPath, exe); err2 != nil {
			return fmt.Errorf("替换二进制失败: %v / %v", err, err2)
		}
		tmpPath = "" // 已就位，勿删
	} else {
		if err := os.Rename(tmpPath, exe); err != nil {
			_ = os.Rename(backup, exe)
			return fmt.Errorf("替换二进制失败: %w", err)
		}
		tmpPath = ""
		_ = os.Remove(backup)
	}

	c.setProgress(PhaseRestarting, asset.Size, asset.Size, "即将重启", "")
	c.log.Info("update applied, re-exec",
		zap.String("from", current),
		zap.String("to", latest),
		zap.String("exe", exe),
	)
	unlocked = true
	err = reexec.Self()
	unlocked = false
	return fmt.Errorf("重启失败: %w", err)
}

func (c *Checker) downloadBytes(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mirrorhub/"+version.Version)
	resp, err := c.downloadClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载失败 HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func (c *Checker) downloadFile(ctx context.Context, url, dest string, expectSize int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "mirrorhub/"+version.Version)
	resp, err := c.downloadClient().Do(req)
	if err != nil {
		return fmt.Errorf("下载资产失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载资产失败 HTTP %d", resp.StatusCode)
	}
	total := expectSize
	if total <= 0 && resp.ContentLength > 0 {
		total = resp.ContentLength
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer f.Close()

	limit := int64(512 << 20) // 512MiB 上限
	if total > 0 && total < limit {
		limit = total + 1024
	}

	var done atomic.Int64
	reader := &progressReader{
		r: io.LimitReader(resp.Body, limit),
		onRead: func(n int) {
			cur := done.Add(int64(n))
			c.setProgress(PhaseDownloading, cur, total, "正在下载更新包", "")
		},
	}
	n, err := io.Copy(f, reader)
	if err != nil {
		return err
	}
	if expectSize > 0 && n != expectSize {
		return fmt.Errorf("下载大小不符: expect %d got %d", expectSize, n)
	}
	c.setProgress(PhaseDownloading, n, total, "下载完成", "")
	return nil
}

type progressReader struct {
	r      io.Reader
	onRead func(n int)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 && p.onRead != nil {
		p.onRead(n)
	}
	return n, err
}

func fileSHA256(path string) (string, error) {
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

func parseSHA256Sums(text, assetName string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if name == assetName {
			return fields[0]
		}
	}
	return ""
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	return v
}

// Compare 比较两个版本号（去 v 前缀的宽松 semver）。a>b 返回 1，a<b 返回 -1，相等返回 0。
func Compare(a, b string) int {
	a = normalizeVersion(a)
	b = normalizeVersion(b)
	if a == b {
		return 0
	}
	if a == "dev" {
		return -1
	}
	if b == "dev" {
		return 1
	}
	ap := splitVer(a)
	bp := splitVer(b)
	n := len(ap)
	if len(bp) > n {
		n = len(bp)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(ap) {
			av = ap[i]
		}
		if i < len(bp) {
			bv = bp[i]
		}
		if av > bv {
			return 1
		}
		if av < bv {
			return -1
		}
	}
	return 0
}

func splitVer(v string) []int {
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, _ := strconv.Atoi(p)
		out = append(out, n)
	}
	return out
}
