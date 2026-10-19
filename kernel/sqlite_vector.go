// R25-B · solo 档纯 Go 向量检索（L-232）
//
// 真源：lightmem.md 任务书 R25 §5（B 线）、§0.4（证伪结论）、§9（接口冻结）。
//
// 路线裁定（§0.4 已证伪）：向量以 **BLOB** 存入 SQLite，检索时 **Go 侧暴力余弦**。
// 不引入 sqlite-vec（C 扩展，与 modernc.org/sqlite 纯 Go 驱动不兼容，且破坏 solo
// 档「零外部依赖」承诺、踩红线⑦）。个人档数据量（≤10 万条）下暴力余弦完全够，
// 且零新增依赖、不破红线⑦。
//
// 设计要点：
//   - SQLiteVectorStore 与 backend_sqlite.go 的 sqliteBackend **共享同一 *sql.DB 与
//     mu**（Solo 单连接串行化），不另开连接、不引入第二真源。
//   - 新表 memory_vectors(memory_id PK, embedding BLOB, dim) 只增不改既有 memories 表
//     与任何迁移 SQL（红线：migrations/*.sql 不动；建表语句写在本文件的 ensureSchema，
//     幂等 CREATE TABLE IF NOT EXISTS）。
//   - 余弦公式与 embedder_vector.go 的 CosineSimilarity 逐位一致，保证测试参考实现
//     与产品路径对拍无误（判据 1 往返一致）。
//   - 默认关：sqliteBackend.VectorSearch 在向量表为空时返回与 6cd26e0 逐位一致的降级
//     错误（"downgrade to text"），不返回垃圾结果（判据 4 零回归 + 判据 3 降级路径仍在）。
//
// 只增不改：本文件是 R25-B 新增文件，不修改任何既有 Schema 字段语义。
package kernel

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"sync"
)

// VectorHit 单条向量命中（B 线定义，C/D/E 线不得另造同义结构）。
// 字段至少含 MemoryID、Score；与 §9 接口冻结一致。
type VectorHit struct {
	MemoryID string  `json:"memory_id"`
	Score    float64 `json:"score"`
}

// VectorPutItem 单条批量写入项（B 线 B2，§9 接口冻结）。
// 字段仅 MemoryID 与 Vec；C/D/E 线不得另造同义结构。
type VectorPutItem struct {
	MemoryID string
	Vec      []float32
}

// SQLiteVectorStore solo 档的向量存取（BLOB + Go 侧余弦）。
//
// 线程安全：所有方法经共享的 mu 串行化（与 sqliteBackend 同一把锁），Solo 单连接下
// 天然无并发写锁冲突。vec 以 float32 little-endian 存入 BLOB。
type SQLiteVectorStore struct {
	db *sql.DB
	mu *sync.Mutex // 与 sqliteBackend.mu 同一把锁（共享同一 *sql.DB）
}

// newSQLiteVectorStore 装配向量存储（共享后端的 *sql.DB 与 mu）。
func newSQLiteVectorStore(db *sql.DB, mu *sync.Mutex) *SQLiteVectorStore {
	return &SQLiteVectorStore{db: db, mu: mu}
}

// ensureSchema 幂等建表（只增新表，不改既有 memories 表与任何迁移 SQL）。
func (s *SQLiteVectorStore) ensureSchema(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS memory_vectors (
		memory_id TEXT PRIMARY KEY,
		embedding BLOB NOT NULL,
		dim       INTEGER NOT NULL
	)`)
	if err != nil {
		return fmt.Errorf("sqlite vector ensureSchema: %w", err)
	}
	return nil
}

// Put 写入/更新一条向量（memory_id → embedding BLOB）。
// 空 memory_id 或空向量视为非法（避免写入无法检索的脏数据）。
func (s *SQLiteVectorStore) Put(ctx context.Context, memoryID string, vec []float32) error {
	if memoryID == "" {
		return fmt.Errorf("sqlite vector put: memory_id required")
	}
	if len(vec) == 0 {
		return fmt.Errorf("sqlite vector put: empty vector for %s", memoryID)
	}
	blob := floatsToBlob(vec)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_vectors (memory_id, embedding, dim) VALUES (?, ?, ?)
		 ON CONFLICT(memory_id) DO UPDATE SET embedding=excluded.embedding, dim=excluded.dim`,
		memoryID, blob, len(vec)); err != nil {
		return fmt.Errorf("sqlite vector put %s: %w", memoryID, err)
	}
	return nil
}

// PutBatch 单事务批量 upsert（B 线 B2，L-236）。整批在一个事务内提交，失败时整体回滚，
// 避免逐条 Put 在大语料（>800 条）下反复提交、超服务端 30s ctx 须靠重试收敛的问题。
//
// 与 Put 语义一致：ON CONFLICT DO UPDATE 同 id 覆盖写（幂等，不重复增长）；空 batch 视为
// no-op；含空 id / 空向量的非法项立即回滚并报错（不残留脏数据）。
func (s *SQLiteVectorStore) PutBatch(ctx context.Context, items []VectorPutItem) error {
	if len(items) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite vector putbatch begin: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO memory_vectors (memory_id, embedding, dim) VALUES (?, ?, ?)
		 ON CONFLICT(memory_id) DO UPDATE SET embedding=excluded.embedding, dim=excluded.dim`)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("sqlite vector putbatch prepare: %w", err)
	}
	defer stmt.Close()
	for _, it := range items {
		if it.MemoryID == "" {
			tx.Rollback()
			return fmt.Errorf("sqlite vector putbatch: memory_id required")
		}
		if len(it.Vec) == 0 {
			tx.Rollback()
			return fmt.Errorf("sqlite vector putbatch: empty vector for %s", it.MemoryID)
		}
		blob := floatsToBlob(it.Vec)
		if _, err := stmt.ExecContext(ctx, it.MemoryID, blob, len(it.Vec)); err != nil {
			tx.Rollback()
			return fmt.Errorf("sqlite vector putbatch %s: %w", it.MemoryID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite vector putbatch commit: %w", err)
	}
	return nil
}

// Search 暴力余弦 top-k（纯 Go，个人档数据量下足够）。
//
// 语义：遍历向量表，对每条计算与 query 的余弦相似度，按 (score 降序, memory_id 升序)
// 确定性排序取前 topK。与 embedder_vector.go 的 CosineSimilarity 逐位一致，
// 保证判据 1 的「往返一致」对拍成立。
func (s *SQLiteVectorStore) Search(ctx context.Context, vec []float32, topK int) ([]VectorHit, error) {
	if len(vec) == 0 {
		return nil, fmt.Errorf("sqlite vector search: query vector required")
	}
	if topK <= 0 {
		topK = 10
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT memory_id, embedding FROM memory_vectors`)
	if err != nil {
		return nil, fmt.Errorf("sqlite vector query: %w", err)
	}
	defer rows.Close()

	var hits []VectorHit
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("sqlite vector scan: %w", err)
		}
		emb, err := blobToFloats(blob)
		if err != nil {
			return nil, err
		}
		score := CosineSimilarity(vec, emb)
		hits = append(hits, VectorHit{MemoryID: id, Score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite vector rows: %w", err)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].MemoryID < hits[j].MemoryID
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

// SearchAllowed 在 allowedIDs 集合内做暴力余弦 top-k（B 线 B1，L-235）。
//
// 与 Search 的区别：只对在 allowedIDs 中的 memory_id 计算余弦并排序，把「全局 top-k
// 后再过滤可见性」的挤占问题（双租户下少数派被多数派挤占、返回 503 no visible results）
// 上移到余弦排序之前，保证可见性正确。
//
// allowedIDs 为 nil 时等价于全局 Search（判据 3 short-circuit：单租户裸查退回全局路径，
// 行为与 82d31e0 逐位一致，零额外开销）。排序口径（score 降序、memory_id 升序）与 Search
// 完全一致，保证与参考实现对拍无误。
func (s *SQLiteVectorStore) SearchAllowed(ctx context.Context, vec []float32, topK int, allowedIDs map[string]bool) ([]VectorHit, error) {
	if len(vec) == 0 {
		return nil, fmt.Errorf("sqlite vector search: query vector required")
	}
	if topK <= 0 {
		topK = 10
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT memory_id, embedding FROM memory_vectors`)
	if err != nil {
		return nil, fmt.Errorf("sqlite vector query: %w", err)
	}
	defer rows.Close()

	var hits []VectorHit
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("sqlite vector scan: %w", err)
		}
		if allowedIDs != nil && !allowedIDs[id] {
			continue
		}
		emb, err := blobToFloats(blob)
		if err != nil {
			return nil, err
		}
		score := CosineSimilarity(vec, emb)
		hits = append(hits, VectorHit{MemoryID: id, Score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite vector rows: %w", err)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].MemoryID < hits[j].MemoryID
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

// Count 已存向量条数（供 /admin/reconcile 类对账与判据 6 用）。
func (s *SQLiteVectorStore) Count(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_vectors`).Scan(&n); err != nil {
		return 0, fmt.Errorf("sqlite vector count: %w", err)
	}
	return n, nil
}

// existingIDs 返回向量表中已有的 memory_id 集合（供 BackfillVectors 幂等跳过）。
func (s *SQLiteVectorStore) existingIDs(ctx context.Context) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT memory_id FROM memory_vectors`)
	if err != nil {
		return nil, fmt.Errorf("sqlite vector existing: %w", err)
	}
	defer rows.Close()
	set := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sqlite vector existing scan: %w", err)
		}
		set[id] = true
	}
	return set, rows.Err()
}

// ------------------------------------------------------------------
// BLOB 编解码（float32 little-endian）+ 余弦
// ------------------------------------------------------------------

// floatsToBlob 把 []float32 编码为 4 字节/元素的 little-endian BLOB。
func floatsToBlob(vec []float32) []byte {
	blob := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(blob[i*4:], math.Float32bits(v))
	}
	return blob
}

// blobToFloats 从 little-endian BLOB 还原 []float32；长度非 4 的倍数视为损坏。
func blobToFloats(blob []byte) ([]float32, error) {
	if len(blob)%4 != 0 {
		return nil, fmt.Errorf("sqlite vector blob length %d not multiple of 4", len(blob))
	}
	n := len(blob) / 4
	vec := make([]float32, n)
	for i := 0; i < n; i++ {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return vec, nil
}
