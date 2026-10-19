// R24-A 判据①：逐位零回归对拍（差分测试）。
//
// 本文件是**自包含**的：只依赖 R23.3（c908091）已有的符号，不引用 lookup.go 的
// 任何类型或函数。这样它可以被原样复制到 c908091 的干净工作树里编译运行，
// 两份输出逐字节 diff 即为「开关关闭时 A 线零回归」的硬证据。
//
// 用法（两棵树各跑一次，输出路径用 -dump-out 区分）：
//
//	cd <tree>/kernel && go test -count=1 -run TestDumpR24ARecallTrace .
//
// 判据①原文：同一批 query，lookupEnabled=false 与 c908091 的 RecallAndFuse
// JSON 输出逐字节相同。本测试把「当前树开关关闭」与「基线树」两侧的输出都落盘，
// 由 TestDumpR24ARecallTrace 之外的人工 diff 命令比对（因为跨进程、跨树，
// 无法在同一进程内断言）。
package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"
)

// dumpTimeFields 需要归一化的时钟字段（后端 Upsert / trace 用 time.Now 落库，
// 跨进程对拍必然不同，属于「时钟」不是「行为」，必须归一化后再比）。
var dumpTimeFields = []string{"recorded_at", "updated_at", "timestamp", "last_accessed_at", "created_at"}

// normalizeDumpClocks 把 raw JSON 中的时钟字段值替换为固定占位符。
func normalizeDumpClocks(raw []byte) []byte {
	for _, f := range dumpTimeFields {
		re := regexp.MustCompile(`"` + f + `"\s*:\s*"[^"]*"`)
		raw = re.ReplaceAll(raw, []byte(`"`+f+`":"FIXED"`))
	}
	return raw
}

// dumpR24AOut 输出文件路径，可用环境变量 R24A_DUMP_OUT 覆盖。
func dumpR24AOut(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("R24A_DUMP_OUT"); p != "" {
		return p
	}
	t.Skip("未设置 R24A_DUMP_OUT，跳过落盘（差分测试需显式指定输出路径）")
	return ""
}

// dumpSeedCorpus 写入确定性语料：不依赖 time.Now，全部时刻由 base 推导。
// 覆盖 L2/L1/L0/L3 四层 + 三种可见性，确保慢车道的硬过滤与融合都有输入。
func dumpSeedCorpus(t *testing.T, b StorageBackend, n int) {
	t.Helper()
	base := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		ent := fmt.Sprintf("客户%02d", i)
		layer := LayerL2
		vis := VisTenant
		switch i % 4 {
		case 1:
			layer, vis = LayerL2, VisTenant
		case 2:
			layer, vis = LayerL1, VisTenant
		case 3:
			layer, vis = LayerL0, VisPrivate
		default:
			layer, vis = LayerL3, VisTenant
		}
		occ := base.Add(time.Duration(i) * time.Minute)
		m := &Memory{
			MemoryID:          fmt.Sprintf("m%02d", i),
			IdempotencyKey:    fmt.Sprintf("m%02d", i),
			TenantID:          "t1",
			UserID:            "u1",
			Visibility:        vis,
			MemoryKind:        KindFact,
			Fact:              fmt.Sprintf("%s的偏好是事项%02d", ent, i),
			Confidence:        0.9,
			Layer:             layer,
			ReferenceFrame:    ReferenceFrame{Entity: ent, Category: "偏好"},
			Importance:        0.3 + 0.02*float64(i-1),
			RecentAccessScore: 0.5,
			OccurredAt:        &occ,
			Permission:        Permission{Owner: "u1"},
		}
		if err := b.Upsert(context.Background(), m); err != nil {
			t.Fatalf("upsert %s: %v", m.MemoryID, err)
		}
	}
}

// TestDumpR24ARecallTrace 把一批 query 的 RecallAndFuse 结果 JSON 落盘，
// 供与 c908091 干净树的同名输出做逐字节 diff（判据①）。
//
// 确定性保证：①语料时刻固定（不调用 time.Now 作为数据）；
// ②融合基准时刻经 SetRankFusion 注入固定值；③抹掉 Trace.Timestamp
// （该字段本身就是 time.Now，必须归一化，否则任何跨进程对拍都无意义）。
func TestDumpR24ARecallTrace(t *testing.T) {
	out := dumpR24AOut(t)

	b, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	dumpSeedCorpus(t, b, 30)

	// 固定打分基准时刻：与 D.3 北极星口径的 -now-fixed 2026-10-14 对齐。
	fixedNow := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	rf := DefaultRankFusion()
	rf.Now = fixedNow

	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, b)
	srv.SetRankFusion(rf)
	// 注意：不调用 SetLookupEnabled —— 保持默认 false，即「开关关闭」态。
	// 也不调用 SetLookupIndex，等价于未注入索引。

	type record struct {
		Query string          `json:"query"`
		Mode  string          `json:"mode"`
		Fused json.RawMessage `json:"fused"`
		Trace json.RawMessage `json:"trace"`
	}
	var recs []record

	queries := []string{}
	for i := 1; i <= 30; i++ {
		queries = append(queries, fmt.Sprintf("客户%02d的偏好", i))
	}
	// 追加若干边界查询：空串、纯噪声、跨实体属性、历史回溯。
	queries = append(queries, "", "完全无关的一句话", "客户01的预算", "项目X的偏好")
	asOf := "2026-10-01T00:00:00Z"

	ctx := context.Background()
	for _, q := range queries {
		rc := RouterContext{Text: q, TenantID: "t1", UserID: "u1"}
		if q == "客户01的预算" {
			rc.AsOf = &asOf
		}
		res, err := srv.RecallAndFuse(ctx, rc)
		if err != nil {
			t.Fatalf("recall %q: %v", q, err)
		}
		// 归一化 Trace.Timestamp（time.Now 产物），其余字段全部保留比对。
		if res.Trace != nil {
			res.Trace.Timestamp = "FIXED"
		}
		fusedRaw, err := json.Marshal(res.Fused)
		if err != nil {
			t.Fatalf("marshal fused: %v", err)
		}
		traceRaw, err := json.Marshal(res.Trace)
		if err != nil {
			t.Fatalf("marshal trace: %v", err)
		}
		recs = append(recs, record{Query: q, Mode: res.Mode, Fused: fusedRaw, Trace: traceRaw})
	}

	raw, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		t.Fatalf("marshal records: %v", err)
	}
	raw = normalizeDumpClocks(raw)
	if err := os.WriteFile(out, append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", out, err)
	}
	t.Logf("dumped %d records -> %s (%d bytes)", len(recs), out, len(raw)+1)
}
