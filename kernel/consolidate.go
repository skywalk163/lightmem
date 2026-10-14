// 后台整合 worker（§3.13 consolidation）
// 真源：lightmem.md §3.13「后台整合」：定期合并重复记忆、生成 procedure、清理过期、投毒复核。
//
// 设计：
//   - RunOnce 执行一轮整合，返回 ConsolidationReport。
//   - Start 以固定间隔循环（默认 5 分钟；测试用短间隔），由 main.go 在启动时 go Start(ctx)。
//
// 只增不改：本文件不修改 model.go 既有 Schema 字段语义。
package kernel

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// MemoryScanner 可选后端接口：跨租户全量扫描（整合 worker 用）。
type MemoryScanner interface {
	ScanAll(ctx context.Context, max int) ([]*Memory, error)
}

// ConsolidationReport 一轮整合的动作统计（§3.13）。
type ConsolidationReport struct {
	Scanned     int       `json:"scanned"`
	Merged      int       `json:"merged"`
	Procedures  int       `json:"procedures"`
	Expired     int       `json:"expired"`
	Quarantined int       `json:"quarantined"`
	RanAt       time.Time `json:"ran_at"`
	DurationMS  int64     `json:"duration_ms"`
	Notes       []string  `json:"notes,omitempty"`
}

// Consolidator 后台整合 worker。
type Consolidator struct {
	backend  StorageBackend
	interval time.Duration
	maxScan  int
	logf     func(string, ...any)
	// R9-A：每轮整合完成后的回调（nil 表示忽略）。
	onRunOnce OnRunOnce
}

// NewConsolidator 构造整合 worker（默认间隔 5 分钟）。
func NewConsolidator(b StorageBackend) *Consolidator {
	return &Consolidator{backend: b, interval: 5 * time.Minute, maxScan: 5000, logf: log.Printf}
}

// SetInterval 设置扫描间隔（测试可用短间隔）。
func (c *Consolidator) SetInterval(d time.Duration) { c.interval = d }

// OnRunOnce 是每轮整合完成后的回调（R9-A 新增，只增不改）：
// 用于外部（如 main.go 的 ServerMetrics）记录最近一次整合时间戳。
type OnRunOnce func(ConsolidationReport)

// SetOnRunOnce 注入每轮整合完成后的回调；nil 表示忽略（默认无回调）。
func (c *Consolidator) SetOnRunOnce(fn OnRunOnce) { c.onRunOnce = fn }

// RunOnce 执行一轮整合。
func (c *Consolidator) RunOnce(ctx context.Context) (ConsolidationReport, error) {
	start := time.Now()
	rep := ConsolidationReport{RanAt: time.Now().UTC()}
	sc, ok := c.backend.(MemoryScanner)
	if !ok {
		return rep, fmt.Errorf("backend %s does not implement MemoryScanner; consolidation requires it", c.backend.Name())
	}
	max := c.maxScan
	if max <= 0 {
		max = 5000
	}
	ms, err := sc.ScanAll(ctx, max)
	if err != nil {
		return rep, fmt.Errorf("consolidate scan: %w", err)
	}
	rep.Scanned = len(ms)
	now := time.Now().UTC()

	// 组合记忆索引：byID 全量指针表；inCompSet 标记处于组合中的记忆
	// （组合父记忆 Components 非空，或其 ID 出现在某父记忆的 Components 中）。
	// 用于「组合不去重拆散」：合并步骤跳过这些记忆，避免破坏父子绑定。
	byID := make(map[string]*Memory, len(ms))
	inCompSet := make(map[string]bool, len(ms))
	for _, m := range ms {
		if m == nil {
			continue
		}
		byID[m.MemoryID] = m
		if len(m.Components) > 0 {
			inCompSet[m.MemoryID] = true
		}
		for _, cid := range m.Components {
			inCompSet[cid] = true
		}
	}

	// 1) 重复记忆合并：同 idempotency_key 或同 (tenant,user,kind,fact) 取最新版，旧条目标 superseded_by。
	groups := map[string][]*Memory{}
	for _, m := range ms {
		if m == nil {
			continue
		}
		groups[dupKey(m)] = append(groups[dupKey(m)], m)
	}
	for _, g := range groups {
		if len(g) < 2 {
			continue
		}
		// 组合不去重拆散：组合父记忆或子记忆不参与去重合并，保持组合结构完整。
		skip := false
		for _, m := range g {
			if inCompSet[m.MemoryID] {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		sort.SliceStable(g, func(i, j int) bool { return g[i].RecordedAt.After(g[j].RecordedAt) })
		latest := g[0]
		for _, old := range g[1:] {
			if old.MemoryID == latest.MemoryID || old.SupersededBy != nil {
				continue
			}
			sup := latest.MemoryID
			old.SupersededBy = &sup
			if err := c.backend.Upsert(ctx, old); err != nil {
				rep.Notes = append(rep.Notes, "merge upsert failed: "+err.Error())
				continue
			}
			rep.Merged++
		}
	}

	// 1.5) 组合记忆一致性（千脑「组合物体」落地）：父记忆 Components 只保留仍存在且
	// 未被取代（SupersededBy==nil）的子记忆 ID——子记忆被取代/遗忘时父列表同步清理
	// （确定性：固定遍历 byID，仅当确有剔除才 Upsert）。对不含组合的既有数据无操作。
	for _, parent := range byID {
		if len(parent.Components) == 0 {
			continue
		}
		dirty := false
		kept := make([]string, 0, len(parent.Components))
		for _, cid := range parent.Components {
			child, ok := byID[cid]
			if !ok || child.SupersededBy != nil {
				dirty = true
				continue
			}
			kept = append(kept, cid)
		}
		if dirty {
			parent.Components = kept
			if err := c.backend.Upsert(ctx, parent); err != nil {
				rep.Notes = append(rep.Notes, "composition sync failed: "+err.Error())
			} else {
				rep.Notes = append(rep.Notes, "composition sync: parent "+parent.MemoryID)
			}
		}
	}

	// 2) procedure 提炼骨架：同 scene+task 且 ≥3 次采纳 → 生成通用 procedure。
	procGroups := map[string][]*Memory{}
	for _, m := range ms {
		if m == nil || m.MemoryKind != KindProcedure {
			continue
		}
		key := strings.ToLower(m.ReferenceFrame.Scene) + "|" + strings.ToLower(m.ReferenceFrame.Task)
		if key == "|" {
			continue
		}
		procGroups[key] = append(procGroups[key], m)
	}
	existing := map[string]bool{}
	for _, m := range ms {
		if m != nil {
			existing[m.MemoryID] = true
		}
	}
	for key, g := range procGroups {
		adopted := 0
		for _, m := range g {
			if adoptedCount(m) >= 1 {
				adopted++
			}
		}
		if adopted < 3 {
			continue
		}
		genID := "proc-" + sanitizeKey(key) + "-generic"
		if existing[genID] {
			continue
		}
		base := g[0]
		gm := &Memory{
			MemoryID:       genID,
			IdempotencyKey: genID,
			TenantID:       base.TenantID,
			UserID:         base.UserID,
			Visibility:     base.Visibility,
			Permission:     base.Permission,
			MemoryKind:     KindProcedure,
			Fact:           "通用流程（由同 scene+task 的多次采纳 procedure 提炼）：" + base.Fact,
			ReferenceFrame: base.ReferenceFrame,
			Confidence:     0.8,
			Importance:     0.6,
			Source:         "system",
			Version:        1,
			Layer:          LayerL2,
			Provenance:     ProvNormal,
			RecordedAt:     now,
		}
		if err := c.backend.Upsert(ctx, gm); err != nil {
			rep.Notes = append(rep.Notes, "procedure upsert failed: "+err.Error())
			continue
		}
		existing[genID] = true
		rep.Procedures++
	}

	// 3) 过期清理：expire_at 已过 → 标 is_historical。
	for _, m := range ms {
		if m == nil || m.ExpireAt == nil {
			continue
		}
		if now.After(*m.ExpireAt) && !m.IsHistorical {
			m.IsHistorical = true
			if err := c.backend.Upsert(ctx, m); err != nil {
				rep.Notes = append(rep.Notes, "expire upsert failed: "+err.Error())
				continue
			}
			rep.Expired++
		}
	}

	// 4) 投毒复核：provenance=suspect 且 confidence<0.1 → 归档到 L3（requires_confirmation 语义的落地）。
	ls, hasLayer := c.backend.(LayerScanner)
	for _, m := range ms {
		if m == nil {
			continue
		}
		if m.Provenance == ProvInjectionSuspect && m.Confidence < 0.1 && m.Layer != LayerL3 {
			if hasLayer {
				if err := ls.UpdateLayer(ctx, m.MemoryID, LayerL3); err != nil {
					rep.Notes = append(rep.Notes, "quarantine failed: "+err.Error())
					continue
				}
			}
			rep.Quarantined++
		}
	}

	rep.DurationMS = time.Since(start).Milliseconds()
	if c.onRunOnce != nil {
		c.onRunOnce(rep)
	}
	return rep, nil
}

// Start 以后台 goroutine 方式启动循环（阻塞直到 ctx 取消）。
func (c *Consolidator) Start(ctx context.Context) {
	if c.interval <= 0 {
		c.interval = 5 * time.Minute
	}
	t := time.NewTicker(c.interval)
	defer t.Stop()
	c.logf("[consolidate] worker started interval=%s", c.interval)
	for {
		select {
		case <-ctx.Done():
			c.logf("[consolidate] worker stopped: %v", ctx.Err())
			return
		case <-t.C:
			rep, err := c.RunOnce(ctx)
			if err != nil {
				c.logf("[consolidate] run error: %v", err)
				continue
			}
			c.logf("[consolidate] scanned=%d merged=%d procedures=%d expired=%d quarantined=%d dur=%dms",
				rep.Scanned, rep.Merged, rep.Procedures, rep.Expired, rep.Quarantined, rep.DurationMS)
		}
	}
}

func dupKey(m *Memory) string {
	if m.IdempotencyKey != "" {
		return "idem:" + m.TenantID + "|" + m.UserID + "|" + m.IdempotencyKey
	}
	return "fact:" + m.TenantID + "|" + m.UserID + "|" + string(m.MemoryKind) + "|" + strings.TrimSpace(m.Fact)
}

func adoptedCount(m *Memory) int {
	n := 0
	for _, f := range m.FeedbackRecords {
		if f.Status == FbAdopted || f.Status == FbEdited {
			n++
		}
	}
	return n
}

func sanitizeKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
