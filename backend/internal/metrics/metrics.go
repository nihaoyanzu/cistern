package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	RequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cistern_requests_total",
		Help: "Proxy requests",
	}, []string{"platform", "strategy", "cache"})

	CacheUsageRatio = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cistern_cache_usage_ratio",
		Help: "Cache size / max size",
	})
	DiskFreeBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cistern_disk_free_bytes",
		Help: "Free bytes on cache filesystem",
	})
	DownloadResumesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cistern_download_resumes_total",
		Help: "Upstream stream Range resumes",
	})
	ErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cistern_errors_total",
		Help: "Errors by kind",
	}, []string{"kind"})
	InflightGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cistern_inflight",
		Help: "In-flight downloads",
	})
	SchedulerP0Active = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cistern_scheduler_p0_active",
		Help: "Active interactive/resume tasks",
	})
	PrefetchPaused = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cistern_prefetch_paused",
		Help: "1 if prefetch paused",
	})
	QueueWaitSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "cistern_queue_wait_seconds",
		Help:    "Task wait time before start",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 2, 5, 15, 60},
	})
	Tasks = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cistern_tasks",
		Help: "Tasks by status and priority",
	}, []string{"status", "priority"})
)

func init() {
	prometheus.MustRegister(
		RequestsTotal,
		CacheUsageRatio,
		DiskFreeBytes,
		DownloadResumesTotal,
		ErrorsTotal,
		InflightGauge,
		SchedulerP0Active,
		PrefetchPaused,
		QueueWaitSeconds,
		Tasks,
	)
}
