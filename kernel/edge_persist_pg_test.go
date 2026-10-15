// R15-D1：位移边 PostgreSQL 真机往返测试（§5.4.1）。
//
// 真源：lightmem.md §5.4.1 + R14 报告附录 D.6。
// 背景：R14 的 edge_persist 只交付 JSON 落盘真实往返 + 两套 DDL 文本（SQLite 有真实往返、PG 只有接口与 DDL）。
// 本轮补 PG 真机：走 database/sql + 真实连接，覆盖建边 → 软删 → Save → Load → 逐条断言。
//
// 红线遵守：
//   - 凭据全部走环境变量（LIGHTMEM_PG_HOST / LIGHTMEM_PG_TEST_DB），不硬编码；
//   - 无 PG 环境时全部 t.Skip，不失败（红线 12）；
//   - 确定性：固定 memory_id / tenant / user，不依赖 wall clock 断言；
//   - 不调 LLM / 外部 embedder；
//   - 测试数据用例间隔离（每个用例独立 scope）；
//   - DDL 只含 CREATE 不含 ALTER（与 edge_persist_test.go 同口径）。
package kernel

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// ---- PG 连接装配 ----

// pgEdgeTestConfig 持有 PG 连接配置（从环境变量读取）。
type pgEdgeTestConfig struct {
	host     string
	port     int
	user     string
	password string
	dbname   string
}

// loadPgEdgeTestConfigOrNil 从环境变量读取配置；无配置时返回 nil（不调 t.Skip）。
func loadPgEdgeTestConfigOrNil() *pgEdgeTestConfig {
	host := os.Getenv("LIGHTMEM_PG_HOST")
	if host == "" {
		return nil
	}
	port := 5432
	if p := os.Getenv("LIGHTMEM_PG_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}
	user := os.Getenv("LIGHTMEM_PG_USER")
	if user == "" {
		user = "postgres"
	}
	password := os.Getenv("LIGHTMEM_PG_PASSWORD")
	dbname := os.Getenv("LIGHTMEM_PG_TEST_DB")
	if dbname == "" {
		dbname = os.Getenv("LIGHTMEM_PG_DB")
	}
	if dbname == "" {
		return nil
	}
	return &pgEdgeTestConfig{
		host:     host,
		port:     port,
		user:     user,
		password: password,
		dbname:   dbname,
	}
}

// loadPgEdgeTestConfig 从环境变量读取配置；无配置时 t.Skip。
func loadPgEdgeTestConfig(t *testing.T) *pgEdgeTestConfig {
	t.Helper()
	cfg := loadPgEdgeTestConfigOrNil()
	if cfg == nil {
		t.Skip("LIGHTMEM_PG_HOST / LIGHTMEM_PG_TEST_DB not set; skipping real PG edge persist e2e")
	}
	return cfg
}

// openPgEdgeDB 打开一个真实 PG 连接。
func openPgEdgeDB(t *testing.T, cfg *pgEdgeTestConfig) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=disable",
		cfg.host, cfg.port, cfg.user, cfg.dbname)
	if cfg.password != "" {
		dsn += " password=" + cfg.password
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("pg open: %v", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		t.Fatalf("pg ping: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// ---- pgEdgePersistence：PG 版 EdgePersistence 实现（测试私有） ----

// pgEdgePersistence 是 EdgePersistence 的 PG 实现。
// 用 scope 列做用例间隔离，每条用例独立 scope。
type pgEdgePersistence struct {
	db    *sql.DB
	scope string
}

// newPgEdgePersistence 构造 PG 版持久化后端：建表（含 scope 列）+ 唯一索引。
func newPgEdgePersistence(t *testing.T, db *sql.DB, scope string) *pgEdgePersistence {
	t.Helper()
	ddl := `-- displacement_edges（§5.4.1）：只增新表，禁止 ALTER 既有表/索引
CREATE TABLE IF NOT EXISTS displacement_edges (
  id             BIGSERIAL PRIMARY KEY,
  scope          TEXT    NOT NULL,
  from_memory_id TEXT    NOT NULL,
  to_memory_id   TEXT    NOT NULL,
  relation       TEXT    NOT NULL CHECK (relation IN ('entity-alias','causal','task-flow','temporal')),
  weight         DOUBLE PRECISION NOT NULL CHECK (weight >= 0 AND weight <= 1),
  created_at     BIGINT  NOT NULL,
  invalidated_at BIGINT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_disp_edge ON displacement_edges
  (scope, from_memory_id, to_memory_id, relation) WHERE invalidated_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_disp_edge_from ON displacement_edges (scope, from_memory_id);
CREATE INDEX IF NOT EXISTS idx_disp_edge_to ON displacement_edges (scope, to_memory_id);
`
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("PG DDL 执行失败：%v", err)
	}
	return &pgEdgePersistence{db: db, scope: scope}
}

// Load 从 PG 读出并重建 EdgeStore。
// 直接填充 store.edges / store.outIdx，绕过 AddEdge 的时钟与容量治理。
// 时间列以 BIGINT 存 unix ns，保证 created_at / invalidated_at 逐位保真
// （TIMESTAMPTZ 只有微秒精度，会丢掉纳秒，与 §5.4.1 契约不符）。
func (p *pgEdgePersistence) Load(ctx context.Context) (*EdgeStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx,
		"SELECT from_memory_id, to_memory_id, relation, weight, created_at, invalidated_at FROM displacement_edges WHERE scope = $1 ORDER BY created_at, from_memory_id, to_memory_id, relation",
		p.scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	store := NewEdgeStore(DefaultMaxOutEdges, edgePersistZeroClock)
	for rows.Next() {
		var (
			from, to string
			rel      string
			w        float64
			created  int64
			invalid  sql.NullInt64
		)
		if err := rows.Scan(&from, &to, &rel, &w, &created, &invalid); err != nil {
			return nil, err
		}
		e := DisplacementEdge{
			FromMemoryID: from,
			ToMemoryID:   to,
			Relation:     EdgeRelation(rel),
			Weight:       w,
			CreatedAt:    time.Unix(0, created).UTC(),
		}
		if invalid.Valid {
			t := time.Unix(0, invalid.Int64).UTC()
			e.InvalidatedAt = &t
		}
		key := edgeTriple(from, to, EdgeRelation(rel))
		store.edges[key] = e
		store.outIdx[from] = append(store.outIdx[from], key)
	}
	return store, nil
}

// Save 把 EdgeStore 全量写盘：先删本 scope 全部行，再 INSERT 全部边。
// 并发安全：事务开始即取 scope 级 advisory lock 串行化，
// 避免多个并发 Save 在 READ COMMITTED 下互相看不见对方的未提交行、
// 撞上唯一索引（duplicate key）而失败。
func (p *pgEdgePersistence) Save(ctx context.Context, store *EdgeStore) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if store == nil {
		store = NewEdgeStore(DefaultMaxOutEdges, edgePersistZeroClock)
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// scope 级互斥（事务结束自动释放）。
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", p.scope); err != nil {
		return err
	}

	if _, err := tx.Exec("DELETE FROM displacement_edges WHERE scope = $1", p.scope); err != nil {
		return err
	}

	for _, e := range store.edges {
		var invalid interface{}
		if e.InvalidatedAt != nil {
			invalid = e.InvalidatedAt.UTC().UnixNano()
		}
		if _, err := tx.Exec(
			"INSERT INTO displacement_edges (scope, from_memory_id, to_memory_id, relation, weight, created_at, invalidated_at) VALUES ($1, $2, $3, $4, $5, $6, $7)",
			p.scope, e.FromMemoryID, e.ToMemoryID, e.Relation, e.Weight, e.CreatedAt.UTC().UnixNano(), invalid,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DDL 返回建表语句文本（静态，本实现不执行）。
func (p *pgEdgePersistence) DDL() string {
	return `-- displacement_edges（§5.4.1）：只增新表，禁止 ALTER 既有表/索引
CREATE TABLE IF NOT EXISTS displacement_edges (
  id             BIGSERIAL PRIMARY KEY,
  scope          TEXT    NOT NULL,
  from_memory_id TEXT    NOT NULL,
  to_memory_id   TEXT    NOT NULL,
  relation       TEXT    NOT NULL CHECK (relation IN ('entity-alias','causal','task-flow','temporal')),
  weight         DOUBLE PRECISION NOT NULL CHECK (weight >= 0 AND weight <= 1),
  created_at     BIGINT  NOT NULL,
  invalidated_at BIGINT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_disp_edge ON displacement_edges
  (scope, from_memory_id, to_memory_id, relation) WHERE invalidated_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_disp_edge_from ON displacement_edges (scope, from_memory_id);
CREATE INDEX IF NOT EXISTS idx_disp_edge_to ON displacement_edges (scope, to_memory_id);
`
}

// ---- 测试数据构造助手 ----

// pgPersistClock 确定性递增时钟。
func pgPersistClock(t0 time.Time) func() time.Time {
	cur := t0
	return func() time.Time {
		cur = cur.Add(time.Nanosecond)
		return cur
	}
}

// pgPersistStore 构造一个带确定性时钟的 EdgeStore。
func pgPersistStore(t *testing.T) *EdgeStore {
	t.Helper()
	return NewEdgeStore(DefaultMaxOutEdges, pgPersistClock(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)))
}

// pgPersistSample 造 4 条边（四类 relation 各一）并软删其中 1 条。
func pgPersistSample(t *testing.T) *EdgeStore {
	t.Helper()
	s := pgPersistStore(t)
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

// pgPersistAllEdges 把 store.edges 复制到 map 供 DeepEqual。
func pgPersistAllEdges(s *EdgeStore) map[string]DisplacementEdge {
	out := map[string]DisplacementEdge{}
	for k, e := range s.edges {
		out[k] = e
	}
	return out
}

// ---- 用例 ----

// TestEdgePersistPgRoundTrip：建边 → Save → Load → 逐条断言（有效边数、软删边数、四类 relation 计数、权重误差 ≤1e-9）。
func TestEdgePersistPgRoundTrip(t *testing.T) {
	cfg := loadPgEdgeTestConfig(t)
	db := openPgEdgeDB(t, cfg)
	scope := "r15d_roundtrip_" + t.Name()
	p := newPgEdgePersistence(t, db, scope)
	src := pgPersistSample(t)
	ctx := context.Background()

	if err := p.Save(ctx, src); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	got, err := p.Load(ctx)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if !reflect.DeepEqual(pgPersistAllEdges(src), pgPersistAllEdges(got)) {
		t.Fatalf("往返不一致：\n原  %v\n回  %v", pgPersistAllEdges(src), pgPersistAllEdges(got))
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
	// 四类 relation 计数
	relCount := map[EdgeRelation]int{}
	for _, e := range got.edges {
		relCount[e.Relation]++
	}
	for _, r := range []EdgeRelation{RelEntityAlias, RelCausal, RelTaskFlow, RelTemporal} {
		if relCount[r] != 1 {
			t.Errorf("relation %s 计数：want 1 got %d", r, relCount[r])
		}
	}
	// 权重误差 ≤1e-9
	for _, e := range got.edges {
		if e.Weight < 0 || e.Weight > 1 {
			t.Errorf("权重越界：%v", e.Weight)
		}
	}
}

// TestEdgePersistPgSoftDeletePreserved：软删边保留不物理删、invalidated_at 逐位保真。
func TestEdgePersistPgSoftDeletePreserved(t *testing.T) {
	cfg := loadPgEdgeTestConfig(t)
	db := openPgEdgeDB(t, cfg)
	scope := "r15d_softdel_" + t.Name()
	p := newPgEdgePersistence(t, db, scope)
	src := pgPersistSample(t)
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

// TestEdgePersistPgSaveIdempotent：重复 Save 幂等（DELETE + INSERT 全量覆盖，结果逐行一致）。
func TestEdgePersistPgSaveIdempotent(t *testing.T) {
	cfg := loadPgEdgeTestConfig(t)
	db := openPgEdgeDB(t, cfg)
	scope := "r15d_idem_" + t.Name()
	p := newPgEdgePersistence(t, db, scope)
	src := pgPersistSample(t)
	ctx := context.Background()
	if err := p.Save(ctx, src); err != nil {
		t.Fatalf("首次 Save 失败：%v", err)
	}
	// 读回第一次的行数
	var n0 int
	if err := p.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM displacement_edges WHERE scope = $1", p.scope).Scan(&n0); err != nil {
		t.Fatalf("COUNT 失败：%v", err)
	}
	for i := 0; i < 3; i++ {
		if err := p.Save(ctx, src); err != nil {
			t.Fatalf("重复 Save 失败：%v", err)
		}
		var n int
		if err := p.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM displacement_edges WHERE scope = $1", p.scope).Scan(&n); err != nil {
			t.Fatalf("COUNT 失败：%v", err)
		}
		if n != n0 {
			t.Fatalf("第 %d 次 Save 行数不一致：want %d got %d", i+2, n0, n)
		}
	}
}

// TestEdgePersistPgEmptyStore：空表与 nil store 安全。
func TestEdgePersistPgEmptyStore(t *testing.T) {
	cfg := loadPgEdgeTestConfig(t)
	db := openPgEdgeDB(t, cfg)
	scope := "r15d_empty_" + t.Name()
	p := newPgEdgePersistence(t, db, scope)
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

// TestEdgePersistPgLoadMissingScope：scope 不存在时返回空表且不报错。
func TestEdgePersistPgLoadMissingScope(t *testing.T) {
	cfg := loadPgEdgeTestConfig(t)
	db := openPgEdgeDB(t, cfg)
	scope := "r15d_missing_" + t.Name()
	p := newPgEdgePersistence(t, db, scope)
	got, err := p.Load(context.Background())
	if err != nil {
		t.Fatalf("scope 不存在时应返回空表且不报错：%v", err)
	}
	if got.EdgeCount() != 0 {
		t.Fatalf("应为空表，实际 %d", got.EdgeCount())
	}
	if !reflect.DeepEqual(got.Summarize(), EdgeSummary{}) {
		t.Fatalf("空表摘要应全 0：%+v", got.Summarize())
	}
}

// TestEdgePersistPgSummaryCounts：Save → Load 后摘要计数正确。
func TestEdgePersistPgSummaryCounts(t *testing.T) {
	cfg := loadPgEdgeTestConfig(t)
	db := openPgEdgeDB(t, cfg)
	scope := "r15d_summary_" + t.Name()
	p := newPgEdgePersistence(t, db, scope)
	src := pgPersistSample(t)
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

// TestEdgePersistPgWeightPrecision：权重往返误差 ≤1e-9。
func TestEdgePersistPgWeightPrecision(t *testing.T) {
	cfg := loadPgEdgeTestConfig(t)
	db := openPgEdgeDB(t, cfg)
	scope := "r15d_weight_" + t.Name()
	p := newPgEdgePersistence(t, db, scope)
	s := pgPersistStore(t)
	w := 0.123456789
	if _, ok := s.AddEdge("m1", "m2", RelCausal, w); !ok {
		t.Fatalf("建边失败")
	}
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

// TestEdgePersistPgDDLOnlyAdds：PG DDL 只含 CREATE 不含 ALTER/DROP/TRUNCATE，四类 relation 全在 CHECK 中。
func TestEdgePersistPgDDLOnlyAdds(t *testing.T) {
	cfg := loadPgEdgeTestConfig(t)
	db := openPgEdgeDB(t, cfg)
	scope := "r15d_ddl_" + t.Name()
	p := newPgEdgePersistence(t, db, scope)
	ddl := p.DDL()
	// 直接用字符串判定（DDL 为多行文本，containsUpper 做大小写无关包含判定）
	for _, banned := range []string{"ALTER TABLE", "ALTER INDEX", "DROP TABLE", "DROP INDEX", "TRUNCATE"} {
		if containsUpper(ddl, banned) {
			t.Fatalf("PG DDL 含禁用语句 %q（只增不改）：%s", banned, ddl)
		}
	}
	if !containsUpper(ddl, "CREATE TABLE IF NOT EXISTS DISPLACEMENT_EDGES") {
		t.Fatalf("PG DDL 缺少新表：%s", ddl)
	}
	if !containsUpper(ddl, "CREATE UNIQUE INDEX IF NOT EXISTS UQ_DISP_EDGE") {
		t.Fatalf("PG DDL 缺少三元组唯一索引：%s", ddl)
	}
	if !containsUpper(ddl, "WHERE INVALIDATED_AT IS NULL") {
		t.Fatalf("PG DDL 唯一索引须建在有效边上：%s", ddl)
	}
	for _, r := range []EdgeRelation{RelEntityAlias, RelCausal, RelTaskFlow, RelTemporal} {
		if !containsUpper(ddl, "'"+string(r)+"'") {
			t.Fatalf("PG DDL 缺少 relation %s", r)
		}
	}
	// scope 列必须存在
	if !containsUpper(ddl, "SCOPE") {
		t.Fatalf("PG DDL 缺少 scope 列（用例隔离必需）")
	}
}

// containsUpper：大小写不敏感的「可执行语句」包含判定。
// 与生产测试 TestEdgePersistDDLOnlyAdds 同口径：先剔除 `--` 注释行，
// 否则注释里写「禁止 ALTER」会被误判成 DDL 含禁用语句；
// 同时避免把 `-- displacement_edges...` 注释当成缺失新表的原因。
func containsUpper(s, sub string) bool {
	stmts := ""
	for _, line := range strings.Split(strings.ToUpper(s), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		stmts += line + "\n"
	}
	return strings.Contains(stmts, strings.ToUpper(sub))
}

// TestEdgePersistPgScopeIsolation：不同 scope 的边互不可见。
func TestEdgePersistPgScopeIsolation(t *testing.T) {
	cfg := loadPgEdgeTestConfig(t)
	db := openPgEdgeDB(t, cfg)
	scopeA := "r15d_isol_a_" + t.Name()
	scopeB := "r15d_isol_b_" + t.Name()
	pA := newPgEdgePersistence(t, db, scopeA)
	pB := newPgEdgePersistence(t, db, scopeB)
	sA := pgPersistStore(t)
	if _, ok := sA.AddEdge("m1", "m2", RelEntityAlias, EdgeWeightEntityAlias); !ok {
		t.Fatalf("建边失败")
	}
	ctx := context.Background()
	if err := pA.Save(ctx, sA); err != nil {
		t.Fatalf("Save A 失败：%v", err)
	}
	// B 的 scope 里应看不到 A 的边
	gotB, err := pB.Load(ctx)
	if err != nil {
		t.Fatalf("Load B 失败：%v", err)
	}
	if gotB.EdgeCount() != 0 {
		t.Fatalf("scope 隔离失败：B 看到 %d 条边（应为 0）", gotB.EdgeCount())
	}
	// A 的 scope 里应看到自己的边
	gotA, err := pA.Load(ctx)
	if err != nil {
		t.Fatalf("Load A 失败：%v", err)
	}
	if gotA.EdgeCount() != 1 {
		t.Fatalf("scope 隔离失败：A 看到 %d 条边（应为 1）", gotA.EdgeCount())
	}
}

// TestEdgePersistPgUniqueConstraint：同 scope 下三元组唯一（软删行可重复，有效行不可重复）。
func TestEdgePersistPgUniqueConstraint(t *testing.T) {
	cfg := loadPgEdgeTestConfig(t)
	db := openPgEdgeDB(t, cfg)
	scope := "r15d_unique_" + t.Name()
	p := newPgEdgePersistence(t, db, scope)
	s := pgPersistStore(t)
	if _, ok := s.AddEdge("m1", "m2", RelEntityAlias, EdgeWeightEntityAlias); !ok {
		t.Fatalf("建边失败")
	}
	ctx := context.Background()
	if err := p.Save(ctx, s); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	// 同 scope 同三元组再 Save 应幂等（DELETE + INSERT 全量覆盖，不报唯一冲突）
	if err := p.Save(ctx, s); err != nil {
		t.Fatalf("重复 Save 报唯一冲突（DELETE+INSERT 全量覆盖应安全）：%v", err)
	}
	// 软删后同三元组可再建（因为 invalidated_at 非空，唯一索引不覆盖）
	if !s.InvalidateEdge("m1", "m2", RelEntityAlias) {
		t.Fatalf("软删失败")
	}
	if err := p.Save(ctx, s); err != nil {
		t.Fatalf("软删后 Save 失败：%v", err)
	}
	// 再建同三元组（有效）应成功（先软删旧的，再 INSERT 新的）
	s2 := pgPersistStore(t)
	if _, ok := s2.AddEdge("m1", "m2", RelEntityAlias, EdgeWeightEntityAlias); !ok {
		t.Fatalf("建边失败")
	}
	if err := p.Save(ctx, s2); err != nil {
		t.Fatalf("软删后重建同三元组失败：%v", err)
	}
}

// TestEdgePersistPgCreatedAtUnixNsFidelity：created_at / invalidated_at 的 unix ns 逐位保真。
func TestEdgePersistPgCreatedAtUnixNsFidelity(t *testing.T) {
	cfg := loadPgEdgeTestConfig(t)
	db := openPgEdgeDB(t, cfg)
	scope := "r15d_ns_" + t.Name()
	p := newPgEdgePersistence(t, db, scope)
	fixedAt := time.Date(2026, 10, 15, 0, 0, 0, 123456789, time.UTC)
	s2 := NewEdgeStore(DefaultMaxOutEdges, func() time.Time { return fixedAt })
	if _, ok := s2.AddEdge("m1", "m2", RelCausal, 0.5); !ok {
		t.Fatalf("建边失败")
	}
	ctx := context.Background()
	if err := p.Save(ctx, s2); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	got, err := p.Load(ctx)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	var e DisplacementEdge
	for _, ee := range got.edges {
		if ee.FromMemoryID == "m1" && ee.ToMemoryID == "m2" {
			e = ee
			break
		}
	}
	// 逐位保真：与固定时刻的 unix ns 完全相等（TIMESTAMPTZ 只有微秒精度会丢 ns）。
	if e.CreatedAt.UnixNano() != fixedAt.UnixNano() {
		t.Fatalf("created_at unix ns 不保真：want %d got %d", fixedAt.UnixNano(), e.CreatedAt.UnixNano())
	}
	// 纳秒位必须非零保真（若退化成微秒精度，该位会变成 789000 或 0）。
	if e.CreatedAt.Nanosecond() != 123456789 {
		t.Fatalf("created_at 纳秒位不保真：want 123456789 got %d", e.CreatedAt.Nanosecond())
	}
	if !e.CreatedAt.Equal(fixedAt) {
		t.Fatalf("created_at 不保真：want %v got %v", fixedAt, e.CreatedAt)
	}
}

// TestEdgePersistPgConcurrentSave：并发 Save 由 PG 事务串行化，不丢边。
func TestEdgePersistPgConcurrentSave(t *testing.T) {
	cfg := loadPgEdgeTestConfig(t)
	db := openPgEdgeDB(t, cfg)
	scope := "r15d_conc_" + t.Name()
	p := newPgEdgePersistence(t, db, scope)
	src := pgPersistSample(t)
	ctx := context.Background()

	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			errCh <- p.Save(ctx, src)
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("并发 Save 失败：%v", err)
		}
	}
	got, err := p.Load(ctx)
	if err != nil {
		t.Fatalf("并发后 Load 失败：%v", err)
	}
	if got.EdgeCount() != 4 {
		t.Fatalf("并发后计数异常：%d", got.EdgeCount())
	}
}

// TestEdgePersistPgMaxOutPreserved：maxOut 在 PG 实现中不持久化（用 DefaultMaxOutEdges），
// 但 Save/Load 往返后边表内容一致。
func TestEdgePersistPgMaxOutPreserved(t *testing.T) {
	cfg := loadPgEdgeTestConfig(t)
	db := openPgEdgeDB(t, cfg)
	scope := "r15d_maxout_" + t.Name()
	p := newPgEdgePersistence(t, db, scope)
	src := NewEdgeStore(7, pgPersistClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	if _, ok := src.AddEdge("m1", "m2", RelCausal, 0.5); !ok {
		t.Fatalf("建边失败")
	}
	ctx := context.Background()
	if err := p.Save(ctx, src); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	got, err := p.Load(ctx)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	// 边表内容一致
	if !reflect.DeepEqual(pgPersistAllEdges(src), pgPersistAllEdges(got)) {
		t.Fatalf("往返不一致")
	}
	// PG 实现不持久化 maxOut（用 DefaultMaxOutEdges），这是与 FileEdgePersistence 的差异，
	// 但边表内容一致即可。
	if got.maxOut != DefaultMaxOutEdges {
		t.Fatalf("PG 实现用 DefaultMaxOutEdges，got %d", got.maxOut)
	}
}
