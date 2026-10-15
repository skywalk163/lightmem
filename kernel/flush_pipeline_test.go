package kernel

import (
	"reflect"
	"testing"
	"time"
)

// flushSourceStub 内存桩，实现 FlushMemSource（不实现任何 StorageBackend）。
// 用于证明 MapFlushToL1 只做「按 ID 取数 + 映射」，不触碰存储后端、不写库。
type flushSourceStub struct {
	m     map[string]*Memory
	calls int
}

func (s *flushSourceStub) GetL0Memory(id string) (*Memory, bool) {
	s.calls++
	m, ok := s.m[id]
	return m, ok
}

func mkFlushMem(id string, kind MemoryKind, conf float64, recAt time.Time) *Memory {
	return &Memory{
		MemoryID:   id,
		MemoryKind: kind,
		Confidence: conf,
		RecordedAt: recAt,
		Source:     "unit-test",
	}
}

// TestFlushPipelineDefaultConfig 默认映射配置回落 KindFact。
func TestFlushPipelineDefaultConfig(t *testing.T) {
	if got := DefaultFlushMappingConfig().UnknownKindFallback; got != KindFact {
		t.Fatalf("default fallback=%q, want fact", got)
	}
}

// TestFlushPipelineRecordedAtPreserved recorded_at 逐位保留，且原始 Memory 不被改写。
func TestFlushPipelineRecordedAtPreserved(t *testing.T) {
	recAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	m := mkFlushMem("m1", KindEvent, 0.9, recAt)
	src := &flushSourceStub{m: map[string]*Memory{"m1": m}}
	plan := FlushPlan{Flush: true, Reasons: []FlushReason{FlushSessionEnd}, Items: []string{"m1"}}
	recs := MapFlushToL1(plan, src, DefaultFlushMappingConfig())
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if !recs[0].RecordedAt.Equal(recAt) {
		t.Fatalf("RecordedAt=%v, want %v (must be preserved)", recs[0].RecordedAt, recAt)
	}
	if !m.RecordedAt.Equal(recAt) {
		t.Fatal("original Memory.RecordedAt was mutated (must not be)")
	}
}

// TestFlushPipelineLayerViaClassifyLayer layer 走既有 ClassifyLayer，不重写分层逻辑。
func TestFlushPipelineLayerViaClassifyLayer(t *testing.T) {
	factHi := mkFlushMem("f1", KindFact, 0.95, time.Now())     // 高置信、无 valid_until → L2
	procLo := mkFlushMem("p1", KindProcedure, 0.9, time.Now()) // 低访问 → L1
	goal := mkFlushMem("g1", KindGoal, 0.9, time.Now())        // goal → L2
	src := &flushSourceStub{m: map[string]*Memory{factHi.MemoryID: factHi, procLo.MemoryID: procLo, goal.MemoryID: goal}}
	plan := FlushPlan{Flush: true, Items: []string{"f1", "p1", "g1"}}
	recs := MapFlushToL1(plan, src, DefaultFlushMappingConfig())
	byID := map[string]FlushRecord{}
	for _, r := range recs {
		byID[r.MemoryID] = r
	}
	if byID["f1"].Layer != ClassifyLayer(factHi) || byID["f1"].Layer != LayerL2 {
		t.Fatalf("f1 layer=%q, want L2 (via ClassifyLayer)", byID["f1"].Layer)
	}
	if byID["p1"].Layer != ClassifyLayer(procLo) || byID["p1"].Layer != LayerL1 {
		t.Fatalf("p1 layer=%q, want L1 (via ClassifyLayer)", byID["p1"].Layer)
	}
	if byID["g1"].Layer != ClassifyLayer(goal) || byID["g1"].Layer != LayerL2 {
		t.Fatalf("g1 layer=%q, want L2 (via ClassifyLayer)", byID["g1"].Layer)
	}
}

// TestFlushPipelineKindExhaustive memory_kind 映射表穷举全部 5 种 MemoryKind。
func TestFlushPipelineKindExhaustive(t *testing.T) {
	want := map[MemoryKind]MemoryKind{
		KindEvent:      KindFact,
		KindFact:       KindFact,
		KindGoal:       KindGoal,
		KindProcedure:  KindProcedure,
		KindPreference: KindFact,
	}
	recAt := time.Now()
	for in, out := range want {
		m := mkFlushMem("x", in, 0.9, recAt)
		src := &flushSourceStub{m: map[string]*Memory{"x": m}}
		recs := MapFlushToL1(FlushPlan{Items: []string{"x"}}, src, DefaultFlushMappingConfig())
		if len(recs) != 1 {
			t.Fatalf("kind %q: expected 1 record, got %d", in, len(recs))
		}
		if recs[0].TargetKind != out {
			t.Fatalf("kind %q: TargetKind=%q, want %q", in, recs[0].TargetKind, out)
		}
	}
	// 映射表必须穷举：覆盖全部 5 种，且不含未知键。
	if len(L0KindToL1Kind) != len(want) {
		t.Fatalf("L0KindToL1Kind has %d entries, want exhaustive %d", len(L0KindToL1Kind), len(want))
	}
}

// TestFlushPipelineFourTriggers 四类触发（会话结束/话题切换/任务完成/L0 容量）各有映射。
func TestFlushPipelineFourTriggers(t *testing.T) {
	m := mkFlushMem("m1", KindEvent, 0.9, time.Now())
	reasons := []FlushReason{FlushSessionEnd, FlushTopicSwitch, FlushTaskDone, FlushL0Capacity}
	for _, r := range reasons {
		src := &flushSourceStub{m: map[string]*Memory{"m1": m}}
		plan := FlushPlan{Flush: true, Reasons: []FlushReason{r}, Items: []string{"m1"}}
		recs := MapFlushToL1(plan, src, DefaultFlushMappingConfig())
		if len(recs) != 1 {
			t.Fatalf("reason %v: expected 1 record, got %d", r, len(recs))
		}
		if !reflect.DeepEqual(recs[0].Reasons, []FlushReason{r}) {
			t.Fatalf("reason %v: Reasons=%v, want [%v]", r, recs[0].Reasons, r)
		}
		if recs[0].RecordedAt != m.RecordedAt {
			t.Fatalf("reason %v: RecordedAt not preserved", r)
		}
	}
}

// TestFlushPipelineReasonsStable Reasons 按枚举序透传（多触发时顺序稳定）。
func TestFlushPipelineReasonsStable(t *testing.T) {
	m := mkFlushMem("m1", KindEvent, 0.9, time.Now())
	src := &flushSourceStub{m: map[string]*Memory{"m1": m}}
	reasons := []FlushReason{FlushSessionEnd, FlushTopicSwitch, FlushTaskDone, FlushL0Capacity}
	plan := FlushPlan{Flush: true, Reasons: reasons, Items: []string{"m1"}}
	recs := MapFlushToL1(plan, src, DefaultFlushMappingConfig())
	if !reflect.DeepEqual(recs[0].Reasons, reasons) {
		t.Fatalf("Reasons=%v, want stable enum order %v", recs[0].Reasons, reasons)
	}
}

// TestFlushPipelineNoIOStub 接口桩证明只取数不写库：调用次数恰为待映射项数。
func TestFlushPipelineNoIOStub(t *testing.T) {
	m1 := mkFlushMem("m1", KindEvent, 0.9, time.Now())
	m2 := mkFlushMem("m2", KindFact, 0.9, time.Now())
	src := &flushSourceStub{m: map[string]*Memory{"m1": m1, "m2": m2}}
	plan := FlushPlan{Flush: true, Reasons: []FlushReason{FlushL0Capacity}, Items: []string{"m2", "m1"}}
	recs := MapFlushToL1(plan, src, DefaultFlushMappingConfig())
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	if src.calls != 2 {
		t.Fatalf("stub calls=%d, want 2 (only lookup, no write/storage touch)", src.calls)
	}
}

// TestFlushPipelineHoldItemsExcluded HoldItems 显式不入 L1（留 L0）。
func TestFlushPipelineHoldItemsExcluded(t *testing.T) {
	m1 := mkFlushMem("m1", KindEvent, 0.9, time.Now())
	m2 := mkFlushMem("m2", KindEvent, 0.9, time.Now())
	src := &flushSourceStub{m: map[string]*Memory{"m1": m1, "m2": m2}}
	plan := FlushPlan{Flush: true, Reasons: []FlushReason{FlushTopicSwitch}, Items: []string{"m1"}, HoldItems: []string{"m2"}}
	recs := MapFlushToL1(plan, src, DefaultFlushMappingConfig())
	if len(recs) != 1 || recs[0].MemoryID != "m1" {
		t.Fatalf("HoldItems must be excluded, got %+v", recs)
	}
}

// TestFlushPipelineNilSource src==nil 返回 nil（不 panic）。
func TestFlushPipelineNilSource(t *testing.T) {
	recs := MapFlushToL1(FlushPlan{Items: []string{"m1"}}, nil, DefaultFlushMappingConfig())
	if recs != nil {
		t.Fatalf("nil source should return nil, got %+v", recs)
	}
}

// TestFlushPipelineSkipsMissing 查不到的 ID 跳过（不臆造记录）。
func TestFlushPipelineSkipsMissing(t *testing.T) {
	m1 := mkFlushMem("m1", KindEvent, 0.9, time.Now())
	src := &flushSourceStub{m: map[string]*Memory{"m1": m1}} // m2 缺失
	plan := FlushPlan{Flush: true, Items: []string{"m1", "m2"}}
	recs := MapFlushToL1(plan, src, DefaultFlushMappingConfig())
	if len(recs) != 1 || recs[0].MemoryID != "m1" {
		t.Fatalf("missing id must be skipped, got %+v", recs)
	}
}

// TestFlushPipelineNoRandom 无随机：同输入多次调用结构完全一致（reflect.DeepEqual）。
func TestFlushPipelineNoRandom(t *testing.T) {
	m1 := mkFlushMem("m1", KindEvent, 0.9, time.Now())
	m2 := mkFlushMem("m2", KindGoal, 0.9, time.Now())
	src := &flushSourceStub{m: map[string]*Memory{"m1": m1, "m2": m2}}
	plan := FlushPlan{Flush: true, Reasons: []FlushReason{FlushTaskDone}, Items: []string{"m2", "m1"}}
	a := MapFlushToL1(plan, src, DefaultFlushMappingConfig())
	b := MapFlushToL1(plan, src, DefaultFlushMappingConfig())
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("non-deterministic mapping\n a=%+v\n b=%+v", a, b)
	}
}

// TestFlushPipelineDeterministicOrder 输出按 MemoryID 字典序（与输入序无关）。
func TestFlushPipelineDeterministicOrder(t *testing.T) {
	ids := []string{"m3", "m1", "m2"}
	srcMap := map[string]*Memory{}
	for _, id := range ids {
		srcMap[id] = mkFlushMem(id, KindEvent, 0.9, time.Now())
	}
	src := &flushSourceStub{m: srcMap}
	plan := FlushPlan{Flush: true, Items: []string{"m3", "m1", "m2"}}
	recs := MapFlushToL1(plan, src, DefaultFlushMappingConfig())
	wantOrder := []string{"m1", "m2", "m3"}
	gotOrder := make([]string, len(recs))
	for i, r := range recs {
		gotOrder[i] = r.MemoryID
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("order=%v, want lexicographic %v", gotOrder, wantOrder)
	}
}

// TestFlushPipelineEmptyPlanSafe 空 plan（无 Items）安全：返回空切片、不 panic。
func TestFlushPipelineEmptyPlanSafe(t *testing.T) {
	src := &flushSourceStub{m: map[string]*Memory{}}
	recs := MapFlushToL1(FlushPlan{}, src, DefaultFlushMappingConfig())
	if len(recs) != 0 {
		t.Fatalf("empty plan should yield empty records, got %d", len(recs))
	}
}
