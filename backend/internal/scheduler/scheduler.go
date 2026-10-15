package scheduler

import (
	"context"
	"errors"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/livehl/mirrorhub/internal/config"
	pypihandler "github.com/livehl/mirrorhub/internal/handlers/pypi"
	"github.com/livehl/mirrorhub/internal/metrics"
	"github.com/livehl/mirrorhub/internal/ratelimit"
)

type Priority int

const (
	PriorityInteractive Priority = 0
	PriorityResume      Priority = 1
	PriorityPrefetch    Priority = 2
)

func (p Priority) String() string {
	switch p {
	case PriorityInteractive:
		return "P0"
	case PriorityResume:
		return "P1"
	default:
		return "P2"
	}
}

type TaskInfo struct {
	ID        string    `json:"id"`
	Platform  string    `json:"platform"`
	URL       string    `json:"url"`
	Label     string    `json:"label"`
	Detail    string    `json:"detail,omitempty"`
	Priority  string    `json:"priority"`
	Boosted   bool      `json:"boosted"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	QueuedAt  time.Time `json:"queued_at"`
	StartedAt time.Time `json:"started_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	WaitMs    int64     `json:"wait_ms"`
	ElapsedMs int64     `json:"elapsed_ms"` // 下载耗时：running=至今；终态=StartedAt→UpdatedAt
	BytesDone int64     `json:"bytes_done"`
	BytesTotal int64    `json:"bytes_total"`
	Error     string    `json:"error,omitempty"`

	// wantP0：制品交互任务，MarkRunning 时才计入 p0Active 并 pause（preparing/inflight 等待不占 P0）。
	// countsActive：当前是否已计入 p0Active；pause 时把运行中 P2 改标为 P1 不改此标记，避免 End 误减。
	wantP0       bool
	countsActive bool
	lastProgAt   time.Time
}

// 交互空隙短于此时长不恢复预取，避免 pip 索引/包请求间隙把 PrefetchPaused 抖开又抖上。
const prefetchResumeDelay = 3 * time.Second

// 完成态任务保留时长，超时从队列剔除，避免无界增长。
const finishedTaskTTL = 10 * time.Minute

type Scheduler struct {
	mu           sync.Mutex
	tasks        map[string]*TaskInfo
	order        []string
	limiters     *ratelimit.Registry
	cfgFn        func() config.SchedulerConfig
	log          *zap.Logger
	p0Active     atomic.Int64
	p0ByPlatform map[string]int64 // 按平台交互下载计数；resume 仅清 p0==0 的平台
	nextID       atomic.Uint64
	resumeMu     sync.Mutex
	resumeTimer  *time.Timer

	// 任务级 context 取消（交互代理 / 预取均可 Bind）；管理台强制取消时调用
	cancelMu sync.Mutex
	cancels  map[string]context.CancelFunc
}

func New(lim *ratelimit.Registry, cfgFn func() config.SchedulerConfig, log *zap.Logger) *Scheduler {
	return &Scheduler{
		tasks:        map[string]*TaskInfo{},
		p0ByPlatform: map[string]int64{},
		cancels:      map[string]context.CancelFunc{},
		limiters:     lim,
		cfgFn:        cfgFn,
		log:          log,
	}
}

// BindCancel 登记任务可取消函数（请求结束时须 UnbindCancel）。
func (s *Scheduler) BindCancel(taskID string, cancel context.CancelFunc) {
	if taskID == "" || cancel == nil {
		return
	}
	s.cancelMu.Lock()
	s.cancels[taskID] = cancel
	s.cancelMu.Unlock()
}

// UnbindCancel 移除登记，不调用 cancel。
func (s *Scheduler) UnbindCancel(taskID string) {
	if taskID == "" {
		return
	}
	s.cancelMu.Lock()
	delete(s.cancels, taskID)
	s.cancelMu.Unlock()
}

func (s *Scheduler) abortBound(taskID string) {
	s.cancelMu.Lock()
	cancel := s.cancels[taskID]
	delete(s.cancels, taskID)
	s.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Begin 登记任务。holdInteractive 为 true 表示制品交互：仅在 MarkRunning 时计入 p0Active 并暂停预取；
// preparing / inflight 等待阶段不占 P0。索引/metadata 应传 false，避免短请求把预取抖停。
func (s *Scheduler) Begin(platform, rawURL string, prio Priority, boost, holdInteractive bool) string {
	seq := s.nextID.Add(1)
	id := rawURL + "#" + strconv.FormatUint(seq, 10)
	now := time.Now()
	prioLabel := prio.String()
	if boost && prio == PriorityInteractive {
		prioLabel = "P0+"
	}
	label, detail := taskDisplay(rawURL)
	info := &TaskInfo{
		ID:        id,
		Platform:  platform,
		URL:       rawURL,
		Label:     label,
		Detail:    detail,
		Priority:  prioLabel,
		Boosted:   boost,
		Status:    "queued",
		CreatedAt: now,
		QueuedAt:  now,
		UpdatedAt: now,
		WaitMs:    0,
		wantP0:    holdInteractive && prio == PriorityInteractive,
	}
	s.mu.Lock()
	s.tasks[id] = info
	if boost || prio == PriorityInteractive || prio == PriorityResume {
		s.order = append([]string{id}, s.order...)
	} else {
		s.order = append(s.order, id)
	}
	s.refreshTaskMetricsLocked()
	s.mu.Unlock()
	return id
}

// MarkPreparing 将 queued 推进为 preparing（HEAD/鉴权/建连），交互路径避免长时间显示「排队中」。
func (s *Scheduler) MarkPreparing(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || t == nil {
		return
	}
	if t.Status != "queued" {
		return
	}
	now := time.Now()
	t.Status = "preparing"
	t.UpdatedAt = now
	if t.StartedAt.IsZero() {
		t.StartedAt = now
		t.WaitMs = now.Sub(t.QueuedAt).Milliseconds()
	}
	s.refreshTaskMetricsLocked()
}

// MarkRunning 将 queued/preparing/waiting 推进为 running（已占槽并开始传数据）。
// 首次进入 running 且 wantP0 时在持锁内计入 p0Active 并 pause 本平台预取。
func (s *Scheduler) MarkRunning(id string) {
	s.mu.Lock()
	t, ok := s.tasks[id]
	if !ok || t == nil {
		s.mu.Unlock()
		return
	}
	if t.Status != "queued" && t.Status != "preparing" && t.Status != "waiting" {
		s.mu.Unlock()
		return
	}
	now := time.Now()
	t.Status = "running"
	if t.StartedAt.IsZero() {
		t.StartedAt = now
		t.WaitMs = now.Sub(t.QueuedAt).Milliseconds()
	} else if t.WaitMs <= 0 {
		t.WaitMs = now.Sub(t.QueuedAt).Milliseconds()
	}
	t.UpdatedAt = now
	activateP0 := false
	platform := t.Platform
	if t.wantP0 && !t.countsActive {
		t.countsActive = true
		s.incP0Locked(platform)
		activateP0 = true
	}
	s.refreshTaskMetricsLocked()
	s.mu.Unlock()

	if activateP0 {
		s.pausePrefetchIfNeeded(platform)
	}
}

// MarkHit 缓存 HIT 展示为 running，不计 p0Active、不 pause 预取。
func (s *Scheduler) MarkHit(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || t == nil {
		return
	}
	if t.Status != "queued" && t.Status != "preparing" && t.Status != "waiting" {
		return
	}
	now := time.Now()
	t.Status = "running"
	if t.StartedAt.IsZero() {
		t.StartedAt = now
		t.WaitMs = now.Sub(t.QueuedAt).Milliseconds()
	} else if t.WaitMs <= 0 {
		t.WaitMs = now.Sub(t.QueuedAt).Milliseconds()
	}
	t.UpdatedAt = now
	s.refreshTaskMetricsLocked()
}

// MarkWaiting 将 preparing/queued 标为 waiting（inflight 合并等待），不计 p0Active。
func (s *Scheduler) MarkWaiting(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || t == nil {
		return
	}
	if t.Status != "queued" && t.Status != "preparing" {
		return
	}
	now := time.Now()
	t.Status = "waiting"
	t.UpdatedAt = now
	if t.StartedAt.IsZero() {
		t.StartedAt = now
		t.WaitMs = now.Sub(t.QueuedAt).Milliseconds()
	}
	s.refreshTaskMetricsLocked()
}

// UpdateProgress 更新下载进度（节流，避免高频锁竞争）。
func (s *Scheduler) UpdateProgress(id string, done, total int64) {
	if done < 0 {
		done = 0
	}
	if total < 0 {
		total = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || t == nil {
		return
	}
	if t.Status != "queued" && t.Status != "preparing" && t.Status != "running" && t.Status != "waiting" {
		return
	}
	now := time.Now()
	force := total != t.BytesTotal || done >= total && total > 0 || done < t.BytesDone
	if !force && !t.lastProgAt.IsZero() && now.Sub(t.lastProgAt) < 200*time.Millisecond {
		delta := done - t.BytesDone
		if delta >= 0 && delta < 256*1024 {
			return
		}
	}
	t.BytesDone = done
	t.BytesTotal = total
	t.UpdatedAt = now
	t.lastProgAt = now
}

func taskDisplay(rawURL string) (label, detail string) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "-", ""
	}
	if strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://") {
		info := pypihandler.ParseArtifactURL(rawURL, "package")
		if info.Name != "" && info.Version != "" {
			return info.Name + " " + info.Version, info.Filename
		}
		if info.Filename != "" {
			return info.Filename, ""
		}
		if info.Name != "" {
			return info.Name, ""
		}
		if u, err := url.Parse(rawURL); err == nil {
			if lab, det := dockerBlobDisplay(u.Path); lab != "" {
				return lab, det
			}
			if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
				return base, ""
			}
		}
	}
	// 包规格（如 requests>=2）或解析失败项
	return rawURL, ""
}

// dockerBlobDisplay 将 /v2/<repo>/blobs/sha256:… 显示为 repo@sha256:前12位…
func dockerBlobDisplay(p string) (label, detail string) {
	const blobs = "/blobs/"
	i := strings.Index(p, blobs)
	if i < 0 || !strings.HasPrefix(p, "/v2/") {
		return "", ""
	}
	repo := strings.TrimPrefix(p[:i], "/v2/")
	dig := p[i+len(blobs):]
	if repo == "" || dig == "" {
		return "", ""
	}
	short := dig
	if strings.HasPrefix(dig, "sha256:") && len(dig) > 19 {
		short = dig[:19] + "…"
	}
	return repo + "@" + short, dig
}

func (s *Scheduler) incP0Locked(platform string) {
	if s.p0ByPlatform == nil {
		s.p0ByPlatform = map[string]int64{}
	}
	s.p0ByPlatform[platform]++
	n := s.p0Active.Add(1)
	metrics.SchedulerP0Active.Set(float64(n))
}

func (s *Scheduler) releaseActiveLocked(t *TaskInfo) {
	if t == nil || !t.countsActive {
		return
	}
	t.countsActive = false
	plat := t.Platform
	if s.p0ByPlatform != nil {
		if c := s.p0ByPlatform[plat]; c > 1 {
			s.p0ByPlatform[plat] = c - 1
		} else {
			delete(s.p0ByPlatform, plat)
		}
	}
	n := s.p0Active.Add(-1)
	if n < 0 {
		s.p0Active.Store(0)
		n = 0
	}
	metrics.SchedulerP0Active.Set(float64(n))
}

func (s *Scheduler) platformP0Locked(platform string) int64 {
	if s.p0ByPlatform == nil {
		return 0
	}
	return s.p0ByPlatform[platform]
}

func (s *Scheduler) End(id string, err error) {
	s.mu.Lock()
	t, ok := s.tasks[id]
	if ok {
		// 已取消的任务不被后续 End 覆盖成 done/error
		if t.Status == "cancelled" {
			s.releaseActiveLocked(t)
			s.pruneFinishedLocked(time.Now())
			s.refreshTaskMetricsLocked()
			s.mu.Unlock()
			s.resumePrefetchIfIdle()
			return
		}
		t.UpdatedAt = time.Now()
		if err != nil {
			if isCancelErr(err) {
				t.Status = "cancelled"
				t.Error = ""
			} else {
				t.Status = "error"
				t.Error = err.Error()
				metrics.ErrorsTotal.WithLabelValues("task").Inc()
			}
		} else {
			t.Status = "done"
		}
		s.releaseActiveLocked(t)
	}
	s.pruneFinishedLocked(time.Now())
	s.refreshTaskMetricsLocked()
	s.mu.Unlock()
	s.resumePrefetchIfIdle()
}

// pruneFinishedLocked 剔除过期完成态任务（须持 s.mu）。
func (s *Scheduler) pruneFinishedLocked(now time.Time) {
	kept := s.order[:0]
	for _, id := range s.order {
		t, ok := s.tasks[id]
		if !ok || t == nil {
			continue
		}
		if (t.Status == "done" || t.Status == "error" || t.Status == "cancelled") &&
			!t.UpdatedAt.IsZero() && now.Sub(t.UpdatedAt) >= finishedTaskTTL {
			s.releaseActiveLocked(t)
			delete(s.tasks, id)
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
}

func isCancelErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "context canceled") || strings.Contains(msg, "context cancelled")
}

func isPrefetchCancellable(prio string) bool {
	return prio == PriorityPrefetch.String() || prio == PriorityResume.String()
}

// URLOf 按任务 id 取 URL；不存在则 ok=false。
func (s *Scheduler) URLOf(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok || t == nil {
		return "", false
	}
	return t.URL, true
}

func (s *Scheduler) cancelResumeTimer() {
	s.resumeMu.Lock()
	defer s.resumeMu.Unlock()
	if s.resumeTimer != nil {
		s.resumeTimer.Stop()
		s.resumeTimer = nil
	}
}

func (s *Scheduler) pausePrefetchIfNeeded(platform string) {
	cfg := s.cfgFn()
	if cfg.Prefetch.OnInteractive != "pause" {
		return
	}
	s.cancelResumeTimer()
	if pl := s.limiters.Get(platform); pl != nil {
		if changed := pl.SetPrefetchPaused(true); changed {
			s.refreshPrefetchPausedMetric()
			s.log.Info("prefetch paused due to interactive traffic", zap.String("platform", platform))
		}
		// 仅同平台「运行中/准备中」的 P2→P1，避免跨平台拖死预取
		s.mu.Lock()
		for _, t := range s.tasks {
			if t == nil || t.Platform != platform {
				continue
			}
			if t.Status != "running" && t.Status != "preparing" {
				continue
			}
			if t.Priority == PriorityPrefetch.String() {
				t.Priority = PriorityResume.String()
				t.UpdatedAt = time.Now()
			}
		}
		s.refreshTaskMetricsLocked()
		s.mu.Unlock()
	}
}

// PrefetchMayRun 预取任务是否允许继续占槽/读字节。
// P0 暂停由限速器 PrefetchPaused 单独处理；此处只表达同平台 P1 > P2。
func (s *Scheduler) PrefetchMayRun(taskID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[taskID]
	if t == nil {
		return true
	}
	if t.Priority == PriorityResume.String() {
		return true
	}
	if t.Priority == PriorityInteractive.String() || strings.HasPrefix(t.Priority, "P0") {
		return true
	}
	return s.resumeCountForPlatformLocked(t.Platform) == 0
}

func (s *Scheduler) resumeCountLocked() int64 {
	var n int64
	for _, t := range s.tasks {
		if t == nil {
			continue
		}
		if t.Priority != PriorityResume.String() {
			continue
		}
		if t.Status == "queued" || t.Status == "preparing" || t.Status == "running" || t.Status == "waiting" {
			n++
		}
	}
	return n
}

func (s *Scheduler) resumeCountForPlatformLocked(platform string) int64 {
	var n int64
	for _, t := range s.tasks {
		if t == nil || t.Platform != platform {
			continue
		}
		if t.Priority != PriorityResume.String() {
			continue
		}
		if t.Status == "queued" || t.Status == "preparing" || t.Status == "running" || t.Status == "waiting" {
			n++
		}
	}
	return n
}

// ResumePrefetchIfIdle 公开入口：热开 ResumeOnIdle 时武装定时器，尊重各平台 p0。
func (s *Scheduler) ResumePrefetchIfIdle() {
	s.resumePrefetchIfIdle()
}

func (s *Scheduler) resumePrefetchIfIdle() {
	cfg := s.cfgFn()
	if !cfg.Prefetch.ResumeOnIdle {
		return
	}
	// 任一平台可能已 idle：即使全局仍有其他平台 P0，也武装定时器做按平台 resume
	s.resumeMu.Lock()
	defer s.resumeMu.Unlock()
	if s.resumeTimer != nil {
		s.resumeTimer.Stop()
	}
	s.resumeTimer = time.AfterFunc(prefetchResumeDelay, func() {
		s.applyPrefetchResumeIdlePlatforms()
	})
}

// applyPrefetchResumeIdlePlatforms 仅解除 p0ByPlatform[p]==0 的平台 PrefetchPaused。
func (s *Scheduler) applyPrefetchResumeIdlePlatforms() {
	cfg := s.cfgFn()
	if !cfg.Prefetch.ResumeOnIdle {
		return
	}
	s.mu.Lock()
	idle := make([]string, 0)
	for _, snap := range s.limiters.Snapshots() {
		name, _ := snap["platform"].(string)
		if name == "" {
			continue
		}
		if s.platformP0Locked(name) == 0 {
			idle = append(idle, name)
		}
	}
	s.mu.Unlock()
	changedAny := false
	for _, name := range idle {
		if pl := s.limiters.Get(name); pl != nil {
			if pl.SetPrefetchPaused(false) {
				changedAny = true
				if s.log != nil {
					s.log.Info("prefetch resumed after interactive idle", zap.String("platform", name))
				}
			}
		}
	}
	if changedAny {
		s.refreshPrefetchPausedMetric()
	}
}

func (s *Scheduler) refreshPrefetchPausedMetric() {
	var n float64
	for _, snap := range s.limiters.Snapshots() {
		if paused, _ := snap["prefetch_paused"].(bool); paused {
			n++
		}
	}
	metrics.PrefetchPaused.Set(n)
}

func (s *Scheduler) InteractiveActive() int64 {
	return s.p0Active.Load()
}

// ResumeCount 当前仍活跃（queued/preparing/running）的 P1 恢复任务数，非历史累计。
func (s *Scheduler) ResumeCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resumeCountLocked()
}

func (s *Scheduler) List() []TaskInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TaskInfo, 0, len(s.order))
	now := time.Now()
	for i := len(s.order) - 1; i >= 0; i-- {
		if t, ok := s.tasks[s.order[i]]; ok {
			cp := *t
			switch {
			case cp.Status == "queued":
				cp.WaitMs = now.Sub(cp.QueuedAt).Milliseconds()
				cp.ElapsedMs = 0
			case !cp.StartedAt.IsZero():
				if cp.WaitMs <= 0 {
					cp.WaitMs = cp.StartedAt.Sub(cp.QueuedAt).Milliseconds()
				}
				if cp.Status == "running" || cp.Status == "preparing" || cp.Status == "waiting" {
					cp.ElapsedMs = now.Sub(cp.StartedAt).Milliseconds()
				} else {
					end := cp.UpdatedAt
					if end.IsZero() || end.Before(cp.StartedAt) {
						end = now
					}
					cp.ElapsedMs = end.Sub(cp.StartedAt).Milliseconds()
				}
			}
			if cp.WaitMs < 0 {
				cp.WaitMs = 0
			}
			if cp.ElapsedMs < 0 {
				cp.ElapsedMs = 0
			}
			out = append(out, cp)
		}
	}
	return out
}

func (s *Scheduler) refreshTaskMetricsLocked() {
	metrics.Tasks.Reset()
	for _, t := range s.tasks {
		metrics.Tasks.WithLabelValues(t.Status, t.Priority).Inc()
	}
}

func isActiveCancellableStatus(status string) bool {
	return status == "running" || status == "preparing" || status == "queued" || status == "waiting"
}

// CancelPrefetch 仅取消 P1/P2 预取类任务，返回 URL 供上层取消下载 context。
func (s *Scheduler) CancelPrefetch(idOrURL string) (taskURL string, ok bool) {
	return s.cancelTask(idOrURL, true)
}

// CancelTask 取消任意优先级活跃任务（含 P0 交互），并触发已 Bind 的 context cancel。
func (s *Scheduler) CancelTask(idOrURL string) (taskURL string, ok bool) {
	return s.cancelTask(idOrURL, false)
}

func (s *Scheduler) cancelTask(idOrURL string, prefetchOnly bool) (taskURL string, ok bool) {
	s.mu.Lock()
	var abortID string
	mark := func(t *TaskInfo) {
		t.Status = "cancelled"
		t.UpdatedAt = time.Now()
		t.Error = ""
		s.releaseActiveLocked(t)
		abortID = t.ID
	}
	allow := func(prio string) bool {
		if prefetchOnly {
			return isPrefetchCancellable(prio)
		}
		return true
	}
	if t, found := s.tasks[idOrURL]; found {
		if isActiveCancellableStatus(t.Status) && allow(t.Priority) {
			mark(t)
			taskURL, ok = t.URL, true
		}
	} else {
		for _, tid := range s.order {
			tt := s.tasks[tid]
			if tt == nil || tt.URL != idOrURL {
				continue
			}
			if !allow(tt.Priority) {
				continue
			}
			if !isActiveCancellableStatus(tt.Status) {
				continue
			}
			mark(tt)
			taskURL, ok = tt.URL, true
			break
		}
	}
	if ok {
		s.refreshTaskMetricsLocked()
	}
	s.mu.Unlock()
	if ok {
		if abortID != "" {
			s.abortBound(abortID)
		}
		s.resumePrefetchIfIdle()
	}
	return taskURL, ok
}

// ClearFinished 清理已结束任务（done/error/cancelled），返回清理数量。
func (s *Scheduler) ClearFinished() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	kept := s.order[:0]
	for _, id := range s.order {
		t, ok := s.tasks[id]
		if !ok {
			continue
		}
		if t.Status == "done" || t.Status == "error" || t.Status == "cancelled" {
			s.releaseActiveLocked(t) // 防御：异常路径未 End 时避免泄漏
			delete(s.tasks, id)
			n++
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
	s.pruneFinishedLocked(time.Now())
	s.refreshTaskMetricsLocked()
	return n
}
