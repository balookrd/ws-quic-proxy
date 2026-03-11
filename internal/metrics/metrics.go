package metrics

import (
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	ActiveSessions = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "h3ws_proxy_active_sessions",
		Help: "Number of active proxy sessions",
	})
	Accepted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "h3ws_proxy_accepted_total",
		Help: "Accepted RFC9220 sessions",
	})
	Rejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "h3ws_proxy_rejected_total",
		Help: "Rejected requests by reason",
	}, []string{"reason"})
	Errors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "h3ws_proxy_errors_total",
		Help: "Errors by stage",
	}, []string{"stage"})
	Bytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "h3ws_proxy_bytes_total",
		Help: "Bytes forwarded by direction",
	}, []string{"dir"})
	Messages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "h3ws_proxy_messages_total",
		Help: "Messages forwarded by direction and type",
	}, []string{"dir", "type"})
	Frames = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "h3ws_proxy_frames_total",
		Help: "WebSocket frames forwarded by direction and opcode",
	}, []string{"dir", "opcode"})
	MessageSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "h3ws_proxy_message_size_bytes",
		Help:    "Observed message size by direction and type",
		Buckets: []float64{64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 4194304},
	}, []string{"dir", "type"})
	SessionDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "h3ws_proxy_session_duration_seconds",
		Help:    "Proxy session lifetime in seconds",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600, 1800, 3600},
	})
	SessionTrafficBytes = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "h3ws_proxy_session_traffic_bytes",
		Help:    "Total bytes transferred per session by direction",
		Buckets: []float64{512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 4194304, 8388608, 16777216, 33554432, 67108864, 134217728},
	}, []string{"dir"})
	Ctrl = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "h3ws_proxy_control_frames_total",
		Help: "Control frames observed",
	}, []string{"type"})
	OversizeDrops = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "h3ws_proxy_oversize_drops_total",
		Help: "Dropped frames/messages due to size limits",
	}, []string{"kind"})
	PreRequestClose = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "h3ws_proxy_prerequest_close_total",
		Help: "QUIC connections closed before any HTTP request reached handler",
	}, []string{"reason"})
	GoMemAllocBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "h3ws_proxy_go_mem_alloc_bytes",
		Help: "Bytes of allocated heap objects",
	})
	GoHeapInuseBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "h3ws_proxy_go_heap_inuse_bytes",
		Help: "Bytes in in-use heap spans",
	})
	GoHeapIdleBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "h3ws_proxy_go_heap_idle_bytes",
		Help: "Bytes in idle (unused) heap spans",
	})
	GoHeapReleasedBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "h3ws_proxy_go_heap_released_bytes",
		Help: "Bytes of physical memory returned to the OS",
	})
	GoMemSysBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "h3ws_proxy_go_mem_sys_bytes",
		Help: "Bytes obtained from the OS",
	})
	GoGCLastPauseSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "h3ws_proxy_go_gc_last_pause_seconds",
		Help: "Last GC stop-the-world pause duration in seconds",
	})
	GoGCCyclesTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "h3ws_proxy_go_gc_cycles_total",
		Help: "Total completed GC cycles",
	})
)

func init() {
	prometheus.MustRegister(
		ActiveSessions, Accepted, Rejected, Errors,
		Bytes, Messages, Frames, MessageSize,
		SessionDuration, SessionTrafficBytes,
		Ctrl, OversizeDrops, PreRequestClose,
		GoMemAllocBytes, GoHeapInuseBytes, GoHeapIdleBytes,
		GoHeapReleasedBytes, GoMemSysBytes,
		GoGCLastPauseSeconds, GoGCCyclesTotal,
	)
}

func UpdateGoRuntimeMetrics() {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)

	GoMemAllocBytes.Set(float64(stats.Alloc))
	GoHeapInuseBytes.Set(float64(stats.HeapInuse))
	GoHeapIdleBytes.Set(float64(stats.HeapIdle))
	GoHeapReleasedBytes.Set(float64(stats.HeapReleased))
	GoMemSysBytes.Set(float64(stats.Sys))

	lastPauseNs := uint64(0)
	if stats.NumGC > 0 {
		lastPauseNs = stats.PauseNs[(stats.NumGC-1)%uint32(len(stats.PauseNs))]
	}
	GoGCLastPauseSeconds.Set(float64(lastPauseNs) / 1e9)
	GoGCCyclesTotal.Set(float64(stats.NumGC))
}
