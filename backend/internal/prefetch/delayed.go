package prefetch

import (
	"context"
	"time"
)

type delayedItem struct {
	url     string
	attempt int
	after   time.Time
}

var delayedBackoff = []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second, 90 * time.Second, 180 * time.Second}

func (s *Service) enqueueDelayed(rawURL string, attempt int) {
	if attempt >= len(delayedBackoff) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.delayed {
		if d.url == rawURL {
			return
		}
	}
	s.delayed = append(s.delayed, delayedItem{
		url:     rawURL,
		attempt: attempt,
		after:   time.Now().Add(delayedBackoff[attempt]),
	})
}

func (s *Service) drainDelayed(ctx context.Context) {
	now := time.Now()
	s.mu.Lock()
	var ready []delayedItem
	kept := s.delayed[:0]
	for _, d := range s.delayed {
		if !d.after.After(now) {
			ready = append(ready, d)
		} else {
			kept = append(kept, d)
		}
	}
	s.delayed = kept
	s.mu.Unlock()
	for _, d := range ready {
		s.startOneAttempt(ctx, d.url, d.attempt)
	}
}

func (s *Service) clearDelayedMatching(platform string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if platform == "" {
		s.delayed = nil
		return
	}
	kept := s.delayed[:0]
	for _, d := range s.delayed {
		plat, _ := platformForURL(s.cfg.Get(), d.url)
		name := "pypi"
		if plat != nil {
			name = plat.Name()
		}
		if name != platform {
			kept = append(kept, d)
		}
	}
	s.delayed = kept
}
