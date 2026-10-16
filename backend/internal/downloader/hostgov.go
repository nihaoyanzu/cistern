package downloader

import (
	"net/url"
	"sync"
	"time"
)

const (
	govClimbInterval     = 30 * time.Second
	govClimbSuccess      = 8
	govFileClimbInterval = 45 * time.Second
	govFileClimbSuccess  = 12
	govFileInitial       = 2
	govFileCeiling       = 4
	govPressureWindow    = 60 * time.Second
	govStallWindow       = 30 * time.Second
	govStallDense        = 2
)

// HostGovernor 按上游 Host 动态调度分片并发与多文件预取并行。
type HostGovernor struct {
	mu    sync.Mutex
	hosts map[string]*hostState
}

type hostState struct {
	effective         int // 单文件分片并发
	ceiling           int
	successStreak     int
	lastClimb         time.Time
	fileEffective     int // 同 Host 同时预取文件数
	fileCeiling       int
	fileSuccessStreak int
	lastFileClimb     time.Time
	lastPressure      time.Time
	stallTimes        []time.Time
	inflightPrefetch  int
}

func newHostGovernor() *HostGovernor {
	return &HostGovernor{hosts: map[string]*hostState{}}
}

func hostKey(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Host
}

func (g *HostGovernor) state(host string) *hostState {
	if host == "" {
		host = "_"
	}
	st, ok := g.hosts[host]
	if !ok {
		now := time.Now()
		st = &hostState{
			effective:     1,
			fileEffective: govFileInitial,
			fileCeiling:   govFileCeiling,
			lastClimb:     now,
			lastFileClimb: now,
		}
		g.hosts[host] = st
	}
	return st
}

func (st *hostState) normalizeFileSlots() {
	if st.fileCeiling < 1 {
		st.fileCeiling = govFileCeiling
	}
	if st.fileEffective < 1 {
		st.fileEffective = 1
	}
	if st.fileEffective > st.fileCeiling {
		st.fileEffective = st.fileCeiling
	}
}

// Limit 返回当前分片工人上限；ceiling 来自平台配置，effective 从 1 缓慢爬升。
func (g *HostGovernor) Limit(host string, ceiling int) int {
	if g == nil {
		if ceiling <= 0 {
			return 1
		}
		return ceiling
	}
	if ceiling <= 0 {
		ceiling = 8
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(host)
	st.ceiling = ceiling
	if st.effective < 1 {
		st.effective = 1
	}
	if st.effective > ceiling {
		st.effective = ceiling
	}
	return st.effective
}

func (g *HostGovernor) ObserveSuccess(host string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(host)
	if st.ceiling <= 0 {
		st.ceiling = 8
	}
	st.normalizeFileSlots()
	// 承压窗口内不爬升分片与文件槽
	if !st.lastPressure.IsZero() && time.Since(st.lastPressure) < govPressureWindow {
		st.successStreak = 0
		st.fileSuccessStreak = 0
		return
	}
	st.successStreak++
	canTime := time.Since(st.lastClimb) >= govClimbInterval
	canCount := st.successStreak >= govClimbSuccess
	if (canTime || canCount) && st.effective < st.ceiling {
		st.effective++
		st.successStreak = 0
		st.lastClimb = time.Now()
	}

	st.fileSuccessStreak++
	fileTime := time.Since(st.lastFileClimb) >= govFileClimbInterval
	fileCount := st.fileSuccessStreak >= govFileClimbSuccess
	if (fileTime || fileCount) && st.fileEffective < st.fileCeiling {
		st.fileEffective++
		st.fileSuccessStreak = 0
		st.lastFileClimb = time.Now()
	}
}

func (g *HostGovernor) ObserveRateLimit(host string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(host)
	st.normalizeFileSlots()
	st.effective = max(1, st.effective/2)
	st.fileEffective = max(1, st.fileEffective/2)
	st.successStreak = 0
	st.fileSuccessStreak = 0
	now := time.Now()
	st.lastPressure = now
	st.lastClimb = now
	st.lastFileClimb = now
}

func (g *HostGovernor) ObserveStall(host string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(host)
	st.normalizeFileSlots()
	now := time.Now()
	// 清理窗口外 stall
	kept := st.stallTimes[:0]
	for _, t := range st.stallTimes {
		if now.Sub(t) <= govStallWindow {
			kept = append(kept, t)
		}
	}
	st.stallTimes = append(kept, now)
	if len(st.stallTimes) >= govStallDense {
		st.effective = max(1, st.effective/2)
		st.fileEffective = max(1, st.fileEffective/2)
		st.successStreak = 0
		st.fileSuccessStreak = 0
		st.lastPressure = now
		st.lastClimb = now
		st.lastFileClimb = now
		st.stallTimes = nil
	}
}

// TryBeginPrefetch 原子占文件槽；满则返回 false（调用方应入等待队列，不耗 attempt）。
func (g *HostGovernor) TryBeginPrefetch(host string) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(host)
	st.normalizeFileSlots()
	if st.inflightPrefetch >= st.fileEffective {
		return false
	}
	st.inflightPrefetch++
	return true
}

// EndPrefetch 释放同 Host 预取文件槽。
func (g *HostGovernor) EndPrefetch(host string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(host)
	if st.inflightPrefetch > 0 {
		st.inflightPrefetch--
	}
}

// EffectiveForTest 仅测试/诊断：返回分片 effective。
func (g *HostGovernor) EffectiveForTest(host string) int {
	if g == nil {
		return 1
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state(host).effective
}

// FileEffectiveForTest 仅测试/诊断：返回文件并行上限。
func (g *HostGovernor) FileEffectiveForTest(host string) int {
	if g == nil {
		return govFileInitial
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(host)
	st.normalizeFileSlots()
	return st.fileEffective
}
