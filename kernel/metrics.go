// Prometheus 指标（§3.10/§5.3 P4 监控）
// 真源：lightmem.md §3.10「温度分层指标」、§5.3「固有价值」。
//
// 设计：
//   - 定义 Prometheus 指标（检索延迟、命中率、污染率、层容量）。
//   - 提供 MetricsCollector 接口，供 router.go 埋点。
//   - 提供 JSON 格式的 /metrics 端点（兼容 Prometheus 文本格式）。
//
// 只增不改：本文件不修改 model.go 既有 Schema 字段语义。
package kernel

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/lightmem/kernel/i18n"
)

// ------------------------------------------------------------------
// 指标定义
// ------------------------------------------------------------------

// MetricsCollector 收集检索指标（供 router.go 埋点）。
type MetricsCollector struct {
	mu sync.RWMutex

	// 检索延迟（毫秒）
	recallLatencies []int64

	// 命中/未命中
	hits   int64
	misses int64

	// 信号计算耗时（毫秒）
	signalLatencies map[string][]int64

	// 层容量
	layerCounts map[MemoryLayer]int64

	// 污染率
	rejectedCount int64
	totalCount    int64

	// 反馈统计
	adoptedCount          int64
	rejectedCountFeedback int64

	// 生命周期操作
	lifecycleSteps map[string]int64
}

// NewMetricsCollector 创建指标收集器。
func NewMetricsCollector() *MetricsCollector {
	return &MetricsCollector{
		recallLatencies: make([]int64, 0, 1000),
		signalLatencies: make(map[string][]int64),
		layerCounts:     make(map[MemoryLayer]int64),
		lifecycleSteps:  make(map[string]int64),
	}
}

// RecordRecallLatency 记录检索延迟。
func (m *MetricsCollector) RecordRecallLatency(ms int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recallLatencies = append(m.recallLatencies, ms)
	if len(m.recallLatencies) > 1000 {
		m.recallLatencies = m.recallLatencies[1:]
	}
}

// RecordHit 记录命中。
func (m *MetricsCollector) RecordHit() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hits++
}

// RecordMiss 记录未命中。
func (m *MetricsCollector) RecordMiss() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.misses++
}

// RecordSignalLatency 记录信号计算耗时。
func (m *MetricsCollector) RecordSignalLatency(signal string, ms int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signalLatencies[signal] = append(m.signalLatencies[signal], ms)
	if len(m.signalLatencies[signal]) > 1000 {
		m.signalLatencies[signal] = m.signalLatencies[signal][1:]
	}
}

// SetLayerCount 设置层容量。
func (m *MetricsCollector) SetLayerCount(layer MemoryLayer, count int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.layerCounts[layer] = count
}

// RecordPollution 记录污染（被拒绝的记忆）。
func (m *MetricsCollector) RecordPollution(rejected bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.totalCount++
	if rejected {
		m.rejectedCount++
	}
}

// RecordFeedback 记录反馈。
func (m *MetricsCollector) RecordFeedback(adopted, rejected bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if adopted {
		m.adoptedCount++
	}
	if rejected {
		m.rejectedCountFeedback++
	}
}

// RecordLifecycleStep 记录生命周期操作。
func (m *MetricsCollector) RecordLifecycleStep(step string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lifecycleSteps[step]++
}

// ------------------------------------------------------------------
// 指标输出
// ------------------------------------------------------------------

// MetricsSnapshot 指标快照。
type MetricsSnapshot struct {
	RecallLatencyMS float64            `json:"recall_latency_ms"`
	HitRate         float64            `json:"hit_rate"`
	PollutionRate   float64            `json:"pollution_rate"`
	LayerCounts     map[string]int64   `json:"layer_counts"`
	SignalLatencies map[string]float64 `json:"signal_latencies_ms,omitempty"`
	FeedbackStats   map[string]int64   `json:"feedback_stats,omitempty"`
	LifecycleStats  map[string]int64   `json:"lifecycle_stats,omitempty"`
	TotalQueries    int64              `json:"total_queries"`
	TotalMemories   int64              `json:"total_memories"`
}

// Snapshot 获取当前指标快照。
func (m *MetricsCollector) Snapshot() *MetricsSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	snap := &MetricsSnapshot{
		LayerCounts:     make(map[string]int64),
		SignalLatencies: make(map[string]float64),
		FeedbackStats:   make(map[string]int64),
		LifecycleStats:  make(map[string]int64),
	}

	// 检索延迟
	if len(m.recallLatencies) > 0 {
		var sum int64
		for _, l := range m.recallLatencies {
			sum += l
		}
		snap.RecallLatencyMS = float64(sum) / float64(len(m.recallLatencies))
	}

	// 命中率
	total := m.hits + m.misses
	if total > 0 {
		snap.HitRate = float64(m.hits) / float64(total)
	}
	snap.TotalQueries = total

	// 污染率
	if m.totalCount > 0 {
		snap.PollutionRate = float64(m.rejectedCount) / float64(m.totalCount)
	}

	// 层容量
	for layer, count := range m.layerCounts {
		snap.LayerCounts[string(layer)] = count
	}

	// 信号延迟
	for sig, lats := range m.signalLatencies {
		if len(lats) > 0 {
			var sum int64
			for _, l := range lats {
				sum += l
			}
			snap.SignalLatencies[sig] = float64(sum) / float64(len(lats))
		}
	}

	// 反馈统计
	snap.FeedbackStats["adopted"] = m.adoptedCount
	snap.FeedbackStats["rejected"] = m.rejectedCountFeedback

	// 生命周期统计
	for step, count := range m.lifecycleSteps {
		snap.LifecycleStats[step] = count
	}

	// 总记忆数
	for _, count := range m.layerCounts {
		snap.TotalMemories += count
	}

	return snap
}

// PrometheusText 返回 Prometheus 文本格式指标。
func (m *MetricsCollector) PrometheusText() string {
	snap := m.Snapshot()
	var out string

	// 检索延迟
	out += fmt.Sprintf("# HELP lims_recall_latency_ms 检索延迟（毫秒）\n")
	out += fmt.Sprintf("# TYPE lims_recall_latency_ms gauge\n")
	out += fmt.Sprintf("lims_recall_latency_ms %.2f\n", snap.RecallLatencyMS)

	// 命中率
	out += fmt.Sprintf("# HELP lims_hit_rate 检索命中率\n")
	out += fmt.Sprintf("# TYPE lims_hit_rate gauge\n")
	out += fmt.Sprintf("lims_hit_rate %.4f\n", snap.HitRate)

	// 污染率
	out += fmt.Sprintf("# HELP lims_pollution_rate 记忆污染率\n")
	out += fmt.Sprintf("# TYPE lims_pollution_rate gauge\n")
	out += fmt.Sprintf("lims_pollution_rate %.4f\n", snap.PollutionRate)

	// 层容量
	out += fmt.Sprintf("# HELP lims_layer_count 各层记忆数\n")
	out += fmt.Sprintf("# TYPE lims_layer_count gauge\n")
	for layer, count := range snap.LayerCounts {
		out += fmt.Sprintf("lims_layer_count{layer=\"%s\"} %d\n", layer, count)
	}

	// 总记忆数
	out += fmt.Sprintf("# HELP lims_total_memories 总记忆数\n")
	out += fmt.Sprintf("# TYPE lims_total_memories gauge\n")
	out += fmt.Sprintf("lims_total_memories %d\n", snap.TotalMemories)

	// 总查询数
	out += fmt.Sprintf("# HELP lims_total_queries 总查询数\n")
	out += fmt.Sprintf("# TYPE lims_total_queries counter\n")
	out += fmt.Sprintf("lims_total_queries %d\n", snap.TotalQueries)

	// 反馈统计
	out += fmt.Sprintf("# HELP lims_feedback_total 反馈统计\n")
	out += fmt.Sprintf("# TYPE lims_feedback_total counter\n")
	for fb, count := range snap.FeedbackStats {
		out += fmt.Sprintf("lims_feedback_total{type=\"%s\"} %d\n", fb, count)
	}

	// 生命周期统计
	out += fmt.Sprintf("# HELP lims_lifecycle_total 生命周期操作统计\n")
	out += fmt.Sprintf("# TYPE lims_lifecycle_total counter\n")
	for step, count := range snap.LifecycleStats {
		out += fmt.Sprintf("lims_lifecycle_total{step=\"%s\"} %d\n", step, count)
	}

	return out
}

// ------------------------------------------------------------------
// 全局指标实例
// ------------------------------------------------------------------

var (
	globalMetrics *MetricsCollector
	metricsInit   sync.Once
)

// GetMetricsCollector 获取全局指标收集器。
func GetMetricsCollector() *MetricsCollector {
	metricsInit.Do(func() {
		globalMetrics = NewMetricsCollector()
	})
	return globalMetrics
}

// ------------------------------------------------------------------
// 审计日志
// ------------------------------------------------------------------

// AuditEntry 审计日志条目。
type AuditEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Action    string    `json:"action"` // save / update / feedback / forget / lifecycle
	MemoryID  string    `json:"memory_id"`
	TenantID  string    `json:"tenant_id"`
	UserID    string    `json:"user_id"`
	Actor     string    `json:"actor"`
	Detail    string    `json:"detail,omitempty"`
}

// AuditLogger 审计日志记录器。
type AuditLogger struct {
	mu         sync.RWMutex
	entries    []AuditEntry
	maxEntries int
}

// NewAuditLogger 创建审计日志记录器。
func NewAuditLogger() *AuditLogger {
	return &AuditLogger{
		entries:    make([]AuditEntry, 0, 10000),
		maxEntries: 10000,
	}
}

// Log 记录审计日志。
func (a *AuditLogger) Log(action, memoryID, tenantID, userID, actor, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	entry := AuditEntry{
		Timestamp: time.Now().UTC(),
		Action:    action,
		MemoryID:  memoryID,
		TenantID:  tenantID,
		UserID:    userID,
		Actor:     actor,
		Detail:    detail,
	}

	a.entries = append(a.entries, entry)
	if len(a.entries) > a.maxEntries {
		// 保留最近 10000 条
		a.entries = a.entries[a.entriesLen()-a.maxEntries+1:]
	}

	// 同时输出到日志
	log.Printf("[audit] action=%s memory_id=%s tenant=%s user=%s actor=%s detail=%s",
		action, memoryID, tenantID, userID, actor, detail)
}

// GetEntries 获取指定记忆的审计日志。
func (a *AuditLogger) GetEntries(memoryID string) []AuditEntry {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var out []AuditEntry
	for _, e := range a.entries {
		if e.MemoryID == memoryID {
			out = append(out, e)
		}
	}
	return out
}

// GetEntriesByTenant 获取指定租户的审计日志。
func (a *AuditLogger) GetEntriesByTenant(tenantID string) []AuditEntry {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var out []AuditEntry
	for _, e := range a.entries {
		if e.TenantID == tenantID {
			out = append(out, e)
		}
	}
	return out
}

// GetRecentEntries 获取最近 N 条审计日志。
func (a *AuditLogger) GetRecentEntries(n int) []AuditEntry {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if n <= 0 {
		return nil
	}
	if n > len(a.entries) {
		n = len(a.entries)
	}
	return a.entries[len(a.entries)-n:]
}

func (a *AuditLogger) entriesLen() int {
	return len(a.entries)
}

// ------------------------------------------------------------------
// 全局审计日志实例
// ------------------------------------------------------------------

var (
	globalAuditLogger *AuditLogger
	auditLoggerInit   sync.Once
)

// GetAuditLogger 获取全局审计日志记录器。
func GetAuditLogger() *AuditLogger {
	auditLoggerInit.Do(func() {
		globalAuditLogger = NewAuditLogger()
	})
	return globalAuditLogger
}

// ------------------------------------------------------------------
// R8-A（P7-E）i18n 接入样板（只增不改：新增调用点，不改既有返回语义）
// ------------------------------------------------------------------

// AuditLogText 返回「审计动作记录」的 i18n 文案（metrics 日志接入样板）。
// 用法：`log.Printf("%s", AuditLogText(action, memoryID, tenantID, userID, actor, detail))`。
// 只增不改：本函数为新增导出，不修改 AuditLogger.Log 等既有函数签名/返回值。
func AuditLogText(action, memoryID, tenantID, userID, actor, detail string) string {
	return T(i18n.MsgAuditAction, action, memoryID, tenantID, userID, actor, detail)
}

// AuditForgetText 返回「审计遗忘记录」的 i18n 文案。
// 用法：`log.Printf("%s", AuditForgetText(memoryID, tenantID, viewer, reason))`。
func AuditForgetText(memoryID, tenantID, viewer, reason string) string {
	return T(i18n.MsgAuditForget, memoryID, tenantID, viewer, reason)
}

// FeedbackCorrectionText 返回「反馈修正候选」的 i18n 文案。
func FeedbackCorrectionText(memoryID, rejectedReason string) string {
	return T(i18n.MsgFeedbackCorr, memoryID, rejectedReason)
}
