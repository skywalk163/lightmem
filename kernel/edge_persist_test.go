// R14-C1 测试：位移边持久化（§5.4.1）。
//
// 覆盖：真实往返保真（含 created_at / invalidated_at 逐位）、软删保留、
// 幂等（重复 Save 逐字节一致）、空表与缺文件、损坏文件、并发 Save、
// DDL 只增不改约束、输出确定性、临时文件无残留。
package kernel

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// persistClock 确定性递增时钟：保证每条边的 created_at 互不相同且可预测。
func persistClock(t0 time.Time) func() time.Time {
	cur := t0
	return func() time.Time {
		cur = cur.Add(time.Nanosecond)
		return cur
	}
}

func persistStore(t *testing.T) *EdgeStore {
	t.Helper()
	return NewEdgeStore(DefaultMaxOutEdges, persistClock(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)))
}

// persistPath 在工作目录建一个持久化文件路径。
func persistPath(t *testing.T) *FileEdgePersistence {
	t.Helper()
	p, err := NewFileEdgePersistence(filepath.Join(t.TempDir(), "edges.json"), DefaultMaxOutEdges)
	if err != nil {
		t.Fatalf("构造持久化后端失败：%v", err)
	}
	return p
}

// persistSample 造 4 条边（四类 relation 各一）并软删其中 1 条。
func persistSample(t *testing.T) *EdgeStore {
	t.Helper()
	s := persistStore(t)
	adds := []struct {
		from, to string
		rel      EdgeRelation
		w        float64
	}{
		{"m1", "m2", RelEntityAlias, EdgeWeightEntityAlias},
		{"m1", "m3", RelCausal, EdgeWeightCausal},
		{"m1", "m4", RelTaskFlow, EdgeWeightTaskFlow},
		{"m1", "m5", RelTemporal, EdgeWeightTemporal},
	}
	for _, a := range adds {
		if _, ok := s.AddEdge(a.from, a.to, a.rel, a.w); !ok {
			t.Fatalf("建边失败：%s -> %s (%s)", a.from, a.to, a.rel)
		}
	}
	if !s.InvalidateEdge("m1", "m3", RelCausal) {
		t.Fatalf("软删失败")
	}
	return s
}

func persistAllEdges(s *EdgeStore) map[string]DisplacementEdge {
	out := map[string]DisplacementEdge{}
	for k, e := range s.edges {
		out[k] = e
	}
	return out
}

func TestEdgePersistFileRoundTrip(t *testing.T) {
	src := persistSample(t)
	p := persistPath(t)
	ctx := context.Background()

	if err := p.Save(ctx, src); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	got, err := p.Load(ctx)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if !reflect.DeepEqual(persistAllEdges(src), persistAllEdges(got)) {
		t.Fatalf("往返不一致：\n原  %v\n回  %v", persistAllEdges(src), persistAllEdges(got))
	}
	if got.EdgeCount() != 4 || got.AliveEdgeCount() != 3 {
		t.Fatalf("计数不一致：total=%d alive=%d", got.EdgeCount(), got.AliveEdgeCount())
	}
	// created_at / invalidated_at 必须逐位保真（不能因重放而漂移）。
	var srcEdge, gotEdge DisplacementEdge
	for _, e := range src.edges {
		if e.FromMemoryID == "m1" && e.ToMemoryID == "m5" {
			srcEdge = e
		}
	}
	for _, e := range got.edges {
		if e.FromMemoryID == "m1" && e.ToMemoryID == "m5" {
			gotEdge = e
		}
	}
	if !srcEdge.CreatedAt.Equal(gotEdge.CreatedAt) || srcEdge.CreatedAt.UnixNano() != gotEdge.CreatedAt.UnixNano() {
		t.Fatalf("created_at 未保真：%v vs %v", srcEdge.CreatedAt, gotEdge.CreatedAt)
	}
}

func TestEdgePersistSoftDeletePreserved(t *testing.T) {
	src := persistSample(t)
	p := persistPath(t)
	ctx := context.Background()
	if err := p.Save(ctx, src); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	got, err := p.Load(ctx)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	key := edgeTriple("m1", "m3", RelCausal)
	ge, ok := got.edges[key]
	if !ok {
		t.Fatalf("软删边丢失（必须保留可审计）")
	}
	if ge.InvalidatedAt == nil {
		t.Fatalf("软删标记丢失：%+v", ge)
	}
	se, _ := src.edges[key]
	if !se.InvalidatedAt.Equal(*ge.InvalidatedAt) {
		t.Fatalf("invalidated_at 未保真：%v vs %v", *se.InvalidatedAt, *ge.InvalidatedAt)
	}
	// 软删边不得出现在出边列表中。
	for _, e := range got.OutEdges("m1") {
		if e.FromMemoryID == "m1" && e.ToMemoryID == "m3" {
			t.Fatalf("软删边出现在 OutEdges 中：%+v", e)
		}
	}
}

func TestEdgePersistSaveIdempotentByteIdentical(t *testing.T) {
	src := persistSample(t)
	p := persistPath(t)
	ctx := context.Background()
	if err := p.Save(ctx, src); err != nil {
		t.Fatalf("首次 Save 失败：%v", err)
	}
	first, err := os.ReadFile(p.Path())
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	for i := 0; i < 3; i++ {
		if err := p.Save(ctx, src); err != nil {
			t.Fatalf("重复 Save 失败：%v", err)
		}
		again, err := os.ReadFile(p.Path())
		if err != nil {
			t.Fatalf("读回失败：%v", err)
		}
		if string(first) != string(again) {
			t.Fatalf("第 %d 次 Save 输出不一致，幂等破坏", i+2)
		}
	}
}

func TestEdgePersistEmptyStore(t *testing.T) {
	p := persistPath(t)
	ctx := context.Background()
	if err := p.Save(ctx, NewEdgeStore(DefaultMaxOutEdges, nil)); err != nil {
		t.Fatalf("空表 Save 失败：%v", err)
	}
	got, err := p.Load(ctx)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if got.EdgeCount() != 0 {
		t.Fatalf("空表往返后应为 0 条，实际 %d", got.EdgeCount())
	}
	if err := p.Save(ctx, nil); err != nil {
		t.Fatalf("nil store Save 应安全：%v", err)
	}
}

func TestEdgePersistLoadMissingFile(t *testing.T) {
	p := persistPath(t)
	got, err := p.Load(context.Background())
	if err != nil {
		t.Fatalf("文件不存在时应返回空表且不报错：%v", err)
	}
	if got.EdgeCount() != 0 {
		t.Fatalf("应为空表，实际 %d", got.EdgeCount())
	}
	if !reflect.DeepEqual(got.Summarize(), EdgeSummary{}) {
		t.Fatalf("空表摘要应全 0：%+v", got.Summarize())
	}
}

func TestEdgePersistSummaryCounts(t *testing.T) {
	src := persistSample(t)
	p := persistPath(t)
	ctx := context.Background()
	if err := p.Save(ctx, src); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	got, err := p.Load(ctx)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	want := EdgeSummary{TotalRows: 4, AliveRows: 3, InvalidatedRows: 1}
	if got.Summarize() != want {
		t.Fatalf("摘要不一致：want %+v got %+v", want, got.Summarize())
	}
}

func TestEdgePersistWeightPrecision(t *testing.T) {
	s := persistStore(t)
	w := 0.123456789
	if _, ok := s.AddEdge("m1", "m2", RelCausal, w); !ok {
		t.Fatalf("建边失败")
	}
	p := persistPath(t)
	ctx := context.Background()
	if err := p.Save(ctx, s); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	got, err := p.Load(ctx)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	var g DisplacementEdge
	found := false
	for _, e := range got.edges {
		if e.Relation == RelCausal {
			g, found = e, true
		}
	}
	if !found {
		t.Fatalf("权重边丢失")
	}
	if diff := g.Weight - w; diff < 0 && diff > -1e-9 || diff > 1e-9 {
		t.Fatalf("权重精度不足：want %v got %v diff %v", w, g.Weight, diff)
	}
}

func TestEdgePersistConcurrentSave(t *testing.T) {
	src := persistSample(t)
	p := persistPath(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Save(context.Background(), src); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("并发 Save 失败：%v", e)
	}
	got, err := p.Load(context.Background())
	if err != nil {
		t.Fatalf("并发后 Load 失败：%v", err)
	}
	if got.EdgeCount() != 4 {
		t.Fatalf("并发后计数异常：%d", got.EdgeCount())
	}
}

func TestEdgePersistDDLOnlyAdds(t *testing.T) {
	for name, ddl := range map[string]string{
		"sqlite": EdgePersistenceDDLSQLite(),
		"pg":     EdgePersistenceDDLPostgres(),
	} {
		upper := strings.ToUpper(ddl)
		// 判定对象只取可执行语句：注释里写「禁止 ALTER」是合法说明，
		// 若直接 Contains("ALTER") 会把注释误判成改动既有表。
		stmts := ""
		for _, line := range strings.Split(upper, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "--") {
				continue
			}
			stmts += line + "\n"
		}
		for _, banned := range []string{"ALTER TABLE", "ALTER INDEX", "DROP TABLE", "DROP INDEX", "TRUNCATE"} {
			if strings.Contains(stmts, banned) {
				t.Fatalf("%s DDL 含禁用语句 %q（只增不改）：%s", name, banned, ddl)
			}
		}
		if !strings.Contains(upper, "CREATE TABLE IF NOT EXISTS DISPLACEMENT_EDGES") {
			t.Fatalf("%s DDL 缺少新表：%s", name, ddl)
		}
		if !strings.Contains(upper, "CREATE UNIQUE INDEX IF NOT EXISTS UQ_DISP_EDGE") {
			t.Fatalf("%s DDL 缺少三元组唯一索引：%s", name, ddl)
		}
		if !strings.Contains(upper, "WHERE INVALIDATED_AT IS NULL") {
			t.Fatalf("%s DDL 唯一索引须建在有效边上：%s", name, ddl)
		}
	}
	// 四类 relation 必须全部进入 CHECK 约束。
	sqlite := EdgePersistenceDDLSQLite()
	for _, r := range []EdgeRelation{RelEntityAlias, RelCausal, RelTaskFlow, RelTemporal} {
		if !strings.Contains(sqlite, "'"+string(r)+"'") {
			t.Fatalf("DDL 缺少 relation %s", r)
		}
	}
}

func TestEdgePersistFileStableOrder(t *testing.T) {
	p1 := persistPath(t)
	p2 := persistPath(t)
	ctx := context.Background()

	// 固定时钟：若用递增时钟，建边顺序不同会让两个 store 的 created_at
	// 天然不同，文件不可能逐字节一致，也就测不出排序确定性。固定时钟下
	// 才真正检验「同内容 → 同输出」。
	fixedAt := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	fixedClock := func() time.Time { return fixedAt }
	s1 := NewEdgeStore(DefaultMaxOutEdges, fixedClock)
	for _, a := range []struct {
		from, to string
		rel      EdgeRelation
		w        float64
	}{
		{"m4", "m1", RelTemporal, 0.3},
		{"m2", "m1", RelTaskFlow, 0.4},
		{"m1", "m2", RelEntityAlias, 0.8},
	} {
		if _, ok := s1.AddEdge(a.from, a.to, a.rel, a.w); !ok {
			t.Fatalf("建边失败")
		}
	}
	s2 := NewEdgeStore(DefaultMaxOutEdges, fixedClock)
	for _, a := range []struct {
		from, to string
		rel      EdgeRelation
		w        float64
	}{
		{"m1", "m2", RelEntityAlias, 0.8},
		{"m2", "m1", RelTaskFlow, 0.4},
		{"m4", "m1", RelTemporal, 0.3},
	} {
		if _, ok := s2.AddEdge(a.from, a.to, a.rel, a.w); !ok {
			t.Fatalf("建边失败")
		}
	}
	if err := p1.Save(ctx, s1); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	if err := p2.Save(ctx, s2); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	b1, err := os.ReadFile(p1.Path())
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	b2, err := os.ReadFile(p2.Path())
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("建边顺序不同导致文件不一致（确定性破坏）：\n%s\n---\n%s", b1, b2)
	}
}

func TestEdgePersistLoadCorruptFile(t *testing.T) {
	p := persistPath(t)
	if err := os.WriteFile(p.Path(), []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if _, err := p.Load(context.Background()); err == nil {
		t.Fatalf("损坏文件应报错")
	}
}

func TestEdgePersistNoTmpLeftover(t *testing.T) {
	src := persistSample(t)
	p := persistPath(t)
	if err := p.Save(context.Background(), src); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	left, err := filepath.Glob(p.Path() + ".tmp*")
	if err != nil {
		t.Fatalf("glob 失败：%v", err)
	}
	if len(left) != 0 {
		t.Fatalf("残留临时文件：%v", left)
	}
}

func TestEdgePersistConstructorGuards(t *testing.T) {
	if _, err := NewFileEdgePersistence("", 0); err == nil {
		t.Fatalf("空路径应报错")
	}
	p, err := NewFileEdgePersistence(filepath.Join(t.TempDir(), "e.json"), 0)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if p.maxOut != DefaultMaxOutEdges {
		t.Fatalf("maxOut 应回落默认 %d，实际 %d", DefaultMaxOutEdges, p.maxOut)
	}
	if p.DDL() != EdgePersistenceDDLSQLite() {
		t.Fatalf("DDL() 应返回 SQLite 方言")
	}
	if p.Path() == "" {
		t.Fatalf("Path() 不应为空")
	}
}

func TestEdgePersistMaxOutPreserved(t *testing.T) {
	p := persistPath(t)
	src := NewEdgeStore(7, persistClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	if _, ok := src.AddEdge("m1", "m2", RelCausal, 0.5); !ok {
		t.Fatalf("建边失败")
	}
	if err := p.Save(context.Background(), src); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	got, err := p.Load(context.Background())
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if got.maxOut != 7 {
		t.Fatalf("maxOut 未保真：want 7 got %d", got.maxOut)
	}
}
