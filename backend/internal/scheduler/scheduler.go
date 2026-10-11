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

	// countsActive 表示 Begin 时是否计入 p0Active（仅 P0 交互）。
	// 暂停时把运行中 P2 改标为 P1 不改此标记，避免 End 误减。
	countsActive bool
	lastProgAt   time.Time
}

// 交互空隙短于此时长不恢复预取，避免 pip 索引/包请求间隙把 PrefetchPaused 抖开又抖上。
const prefetchResumeDelay = 3 * time.Second

type Scheduler struct {
	mu           sync.Mutex
	tasks        map[string]*TaskInfo
	order        []string
	limiters     *ratelimit.Registry
	cfgFn        func() config.SchedulerConfig
	log          *zap.Logger
	p0Active     atomic.Int64
	nextID       atomic.Uint64
	resumeMu     sync.Mutex
	resumeTimer  *time.Timer
}

func New(lim *ratelimit.Registry, cfgFn func() config.SchedulerConfig, log *zap.Logger) *Scheduler {
	return &Scheduler{
		tasks:    map[string]*TaskInfo{},
		limiters: lim,
		cfgFn:    cfgFn,
		log:      log,
	}
}

// Begin 登记任务。holdInteractive 为 true 时才计入 p0Active 并暂停预取
//（制品下载）；索引/metadata 应传 false，避免短请求把预取抖停。
func (s *Scheduler) Begin(platform, rawURL string, prio Priority, boost, holdInteractive bool) string {
	seq := s.nextID.Add(1)
	id := rawURL + "#" + strconv.FormatUint(seq, 10)
	now := time.Now()
	prioLabel := prio.String()
	if boost && prio == PriorityInteractive {
		prioLabel = "P0+"
	}
	countsActive := holdInteractive && prio == PriorityInteractive
	label, detail := taskDisplay(rawURL)
	info := &TaskInfo{
		ID:           id,
		Platform:     platform,
		URL:          rawURL,
		Label:        label,
		Detail:       detail,
		Priority:     prioLabel,
		Boosted:      boost,
		Status:       "queued",
		CreatedAt:    now,
		QueuedAt:     now,
		UpdatedAt:    now,
		WaitMs:       0,
		countsActive: countsActive,
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

	if countsActive {
		s.p0Active.Add(1)
		metrics.SchedulerP0Active.Set(float64(s.p0Active.Load()))
		s.pausePrefetchIfNeeded(platform)
	}
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

// MarkRunning 将 queued/preparing 推进为 running（已占槽并开始传数据）。
func (s *Scheduler) MarkRunning(id string) {
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
	if t.Status != "queued" && t.Status != "preparing" && t.Status != "running" {
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
			if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
				return base, ""
			}
		}
	}
	// 包规格（如 requests>=2）或解析失败项
	return rawURL, ""
}

func (s *Scheduler) releaseActiveLocked(t *TaskInfo) {
	if t == nil || !t.countsActive {
		return
	}
	t.countsActive = false
	n := s.p0Active.Add(-1)
	if n < 0 {
		s.p0Active.Store(0)
		n = 0
	}
	metrics.SchedulerP0Active.Set(float64(n))
}

func (s *Scheduler) End(id string, err error) {
	s.mu.Lock()
	t, ok := s.tasks[id]
	if ok {
		// 已取消的任务不被后续 End 覆盖成 done/error
		if t.Status == "cancelled" {
			s.releaseActiveLocked(t)
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
	s.refreshTaskMetricsLocked()
	s.mu.Unlock()
	s.resumePrefetchIfIdle()
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
		// 只把「运行中/准备中」的 P2 标成 P1（被打断的在途任务）；排队中的仍保持 P2，避免 P1 膨胀
		s.mu.Lock()
		for _, t := range s.tasks {
			if t == nil || (t.Status != "running" && t.Status != "preparing") {
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
// P0 暂停由限速器 PrefetchPaused 单独处理；此处只表达 P1 > P2：
// 「P1 清空」= 没有仍在 queued/running 的 P1（被打断的那批跑完/取消），不是清队列按钮。
// 仍有活跃 P1 时普通 P2 让路；已被标为 P1 的可在 pause 解除后继续。
func (s *Scheduler) PrefetchMayRun(taskID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[taskID]
	if t == nil {
		return s.resumeCountLocked() == 0
	}
	if t.Priority == PriorityResume.String() {
		return true
	}
	if t.Priority == PriorityInteractive.String() || strings.HasPrefix(t.Priority, "P0") {
		return true
	}
	return s.resumeCountLocked() == 0
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
		if t.Status == "queued" || t.Status == "preparing" || t.Status == "running" {
			n++
		}
	}
	return n
}

func (s *Scheduler) resumePrefetchIfIdle() {
	cfg := s.cfgFn()
	if !cfg.Prefetch.ResumeOnIdle {
		return
	}
	if s.p0Active.Load() > 0 {
		return
	}
	s.resumeMu.Lock()
	defer s.resumeMu.Unlock()
	if s.resumeTimer != nil {
		s.resumeTimer.Stop()
	}
	s.resumeTimer = time.AfterFunc(prefetchResumeDelay, func() {
		if s.p0Active.Load() > 0 {
			return
		}
		s.applyPrefetchResume()
	})
}

func (s *Scheduler) applyPrefetchResume() {
	for _, snap := range s.limiters.Snapshots() {
		name, _ := snap["platform"].(string)
		if pl := s.limiters.Get(name); pl != nil {
			if pl.SetPrefetchPaused(false) {
				if s.log != nil {
					s.log.Info("prefetch resumed after interactive idle", zap.String("platform", name))
				}
			}
		}
	}
	s.refreshPrefetchPausedMetric()
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
				if cp.Status == "running" || cp.Status == "preparing" {
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

// CancelPrefetch 仅取消 P1/P2 预取类任务，返回 URL 供上层取消下载 context。
func (s *Scheduler) CancelPrefetch(idOrURL string) (taskURL string, ok bool) {
	s.mu.Lock()
	mark := func(t *TaskInfo) {
		t.Status = "cancelled"
		t.UpdatedAt = time.Now()
		t.Error = ""
		s.releaseActiveLocked(t)
	}
	if t, found := s.tasks[idOrURL]; found {
		if (t.Status == "running" || t.Status == "preparing" || t.Status == "queued") && isPrefetchCancellable(t.Priority) {
			mark(t)
			taskURL, ok = t.URL, true
		}
	} else {
		for _, tid := range s.order {
			tt := s.tasks[tid]
			if tt == nil || tt.URL != idOrURL {
				continue
			}
			if !isPrefetchCancellable(tt.Priority) {
				continue
			}
			if tt.Status != "running" && tt.Status != "preparing" && tt.Status != "queued" {
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
	s.refreshTaskMetricsLocked()
	return n
}
