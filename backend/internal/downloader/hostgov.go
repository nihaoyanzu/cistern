package downloader

import (
	"net/url"
	"sync"
	"time"
)

const (
	govClimbInterval = 30 * time.Second
	govClimbSuccess  = 8
	govStallWindow   = 30 * time.Second
	govStallDense    = 2
)

// HostGovernor 按上游 Host 动态调度分片并发与预取投放软限。
type HostGovernor struct {
	mu    sync.Mutex
	hosts map[string]*hostState
}

type hostState struct {
	effective        int
	ceiling          int
	successStreak    int
	lastClimb        time.Time
	lastPressure     time.Time
	stallTimes       []time.Time
	inflightPrefetch int
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
		st = &hostState{effective: 1, lastClimb: time.Now()}
		g.hosts[host] = st
	}
	return st
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
	// 承压后短暂窗口内不爬升
	if !st.lastPressure.IsZero() && time.Since(st.lastPressure) < govClimbInterval {
		st.successStreak = 0
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
}

func (g *HostGovernor) ObserveRateLimit(host string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(host)
	st.effective = max(1, st.effective/2)
	st.successStreak = 0
	st.lastPressure = time.Now()
	st.lastClimb = time.Now()
}

func (g *HostGovernor) ObserveStall(host string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(host)
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
		st.successStreak = 0
		st.lastPressure = now
		st.lastClimb = now
		st.stallTimes = nil
	}
}

// pressured 是否处于限流/stall 承压窗口（约 30s）。
func (g *HostGovernor) pressured(host string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(host)
	return !st.lastPressure.IsZero() && time.Since(st.lastPressure) < govClimbInterval
}

// AllowPrefetch 承压时同 Host 新预取最多 1 路。
func (g *HostGovernor) AllowPrefetch(host string) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(host)
	pressured := !st.lastPressure.IsZero() && time.Since(st.lastPressure) < govClimbInterval
	if !pressured {
		return true
	}
	return st.inflightPrefetch < 1
}

// BeginPrefetch / EndPrefetch 跟踪同 Host 预取 inflight。
func (g *HostGovernor) BeginPrefetch(host string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.state(host)
	st.inflightPrefetch++
}

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

// EffectiveForTest 仅测试/诊断。
func (g *HostGovernor) EffectiveForTest(host string) int {
	if g == nil {
		return 1
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state(host).effective
}
