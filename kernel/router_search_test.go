// GET /search 参数口径回归测试（R19-A，缺陷 I-12）。
//
// 覆盖任务书 §三 A 线验收项：q 命中、text 命中、二者结果等价、
// 无匹配返回空集（[] 而非 null）、空查询 400、q 优先于 text、
// q 空时回落 text、tenant 隔离、viewer 私有隔离、kind 过滤、
// top_k、大小写不敏感、两端空白裁剪。
//
// 全部用例经真实 net/http 打 APIServer.Handler()，覆盖
// 路由 → handleSearch → StorageBackend.Query 全链路。
package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newSearchTestEnv 装配一个 SQLite 骨架后端的 APIServer（Solo 档）。
func newSearchTestEnv(t *testing.T) (*APIServer, StorageBackend) {
	t.Helper()
	b, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	return NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, b), b
}

// putSearchMemory 直接写一条记忆（绕过 HTTP，聚焦检索侧断言）。
func putSearchMemory(t *testing.T, b StorageBackend, id, tenant, user string,
	vis Visibility, kind MemoryKind, fact string) {
	t.Helper()
	m := &Memory{
		MemoryID:       id,
		IdempotencyKey: id,
		TenantID:       tenant,
		UserID:         user,
		Visibility:     vis,
		MemoryKind:     kind,
		Fact:           fact,
		Confidence:     0.9,
		Layer:          LayerL1,
		Permission:     Permission{Owner: user},
	}
	if err := b.Upsert(context.Background(), m); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

// searchGET 发一次 GET /search，返回状态码与原始 body。
func searchGET(t *testing.T, srv *APIServer, query url.Values) (int, string) {
	t.Helper()
	target := "/search"
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// searchIDs 断言 200 并解析出 memory_id 列表（保持服务端返回顺序）。
func searchIDs(t *testing.T, srv *APIServer, query url.Values) []string {
	t.Helper()
	code, body := searchGET(t, srv, query)
	if code != http.StatusOK {
		t.Fatalf("search %v: want 200, got %d: %s", query, code, body)
	}
	var res []QueryResult
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("unmarshal search body: %v: %s", err, body)
	}
	ids := make([]string, 0, len(res))
	for _, r := range res {
		ids = append(ids, r.MemoryID)
	}
	return ids
}

// onlyID 断言 ids 恰好是 want 一条。
func onlyID(t *testing.T, ids []string, want string) {
	t.Helper()
	if len(ids) != 1 || ids[0] != want {
		t.Fatalf("want exactly [%s], got %v", want, ids)
	}
}

// hasID 判断 id 是否在 ids 中。
func hasID(ids []string, id string) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

// seedSearchCorpus 写入两条同租户记忆：一条含 ERP，一条不含。
func seedSearchCorpus(t *testing.T, b StorageBackend) {
	t.Helper()
	putSearchMemory(t, b, "m-erp", "t1", "u1", VisTenant, KindFact, "财务部门使用 ERP 系统记账")
	putSearchMemory(t, b, "m-meeting", "t1", "u1", VisTenant, KindFact, "客户A偏好周一上午开周会")
}

// TestSearchQParamFilters q 参数按文本真实过滤（基线行为）。
func TestSearchQParamFilters(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	seedSearchCorpus(t, b)

	ids := searchIDs(t, srv, url.Values{"q": {"ERP"}, "tenant_id": {"t1"}, "viewer": {"u1"}})
	onlyID(t, ids, "m-erp")
}

// TestSearchTextAliasFilters text 别名同样按文本过滤（R160 I-12 的直接回归点）。
func TestSearchTextAliasFilters(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	seedSearchCorpus(t, b)

	ids := searchIDs(t, srv, url.Values{"text": {"ERP"}, "tenant_id": {"t1"}, "viewer": {"u1"}})
	onlyID(t, ids, "m-erp")
}

// TestSearchTextAliasMatchesQ 同一查询串用 q 与 text 得到逐字节相同的响应体。
func TestSearchTextAliasMatchesQ(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	seedSearchCorpus(t, b)

	_, bodyQ := searchGET(t, srv, url.Values{"q": {"ERP"}, "tenant_id": {"t1"}, "viewer": {"u1"}})
	_, bodyText := searchGET(t, srv, url.Values{"text": {"ERP"}, "tenant_id": {"t1"}, "viewer": {"u1"}})
	if bodyQ != bodyText {
		t.Fatalf("q and text must be equivalent:\nq   =%s\ntext=%s", bodyQ, bodyText)
	}
}

// TestSearchNonexistentQueryReturnsEmpty 不存在的串必须返回空集（旧行为是返回全量）。
func TestSearchNonexistentQueryReturnsEmpty(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	seedSearchCorpus(t, b)

	const missing = "完全不存在的查询串-zzz"
	for _, key := range []string{"q", "text"} {
		code, body := searchGET(t, srv, url.Values{key: {missing}, "tenant_id": {"t1"}, "viewer": {"u1"}})
		if code != http.StatusOK {
			t.Fatalf("%s=%q: want 200, got %d: %s", key, missing, code, body)
		}
		if got := strings.TrimSpace(body); got != "[]" {
			t.Fatalf("%s=%q: want empty array [], got %s", key, missing, got)
		}
	}
}

// TestSearchQPrecedenceOverText q 与 text 同时出现时 q 优先。
func TestSearchQPrecedenceOverText(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	seedSearchCorpus(t, b)

	ids := searchIDs(t, srv, url.Values{
		"q":         {"ERP"},
		"text":      {"完全不存在的查询串-zzz"},
		"tenant_id": {"t1"},
		"viewer":    {"u1"},
	})
	onlyID(t, ids, "m-erp")
}

// TestSearchTextFallbackWhenQEmpty q 为空（含显式空串）时回落到 text。
func TestSearchTextFallbackWhenQEmpty(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	seedSearchCorpus(t, b)

	ids := searchIDs(t, srv, url.Values{
		"q":         {""},
		"text":      {"ERP"},
		"tenant_id": {"t1"},
		"viewer":    {"u1"},
	})
	onlyID(t, ids, "m-erp")
}

// TestSearchEmptyQueryRejected 空查询返回 400，不返回全量。
func TestSearchEmptyQueryRejected(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	seedSearchCorpus(t, b)

	cases := []struct {
		name  string
		query url.Values
	}{
		{"无 q 无 text", url.Values{"tenant_id": {"t1"}, "viewer": {"u1"}}},
		{"q 与 text 均为空串", url.Values{"q": {""}, "text": {""}, "tenant_id": {"t1"}, "viewer": {"u1"}}},
		{"q 仅空白", url.Values{"q": {"   "}, "tenant_id": {"t1"}, "viewer": {"u1"}}},
		{"text 仅空白", url.Values{"text": {"\t \n"}, "tenant_id": {"t1"}, "viewer": {"u1"}}},
	}
	for _, tc := range cases {
		code, body := searchGET(t, srv, tc.query)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d: %s", tc.name, code, body)
		}
		if !strings.Contains(body, "q or text required") {
			t.Fatalf("%s: want error 'q or text required', got %s", tc.name, body)
		}
	}
}

// TestSearchMissingTenantRejected 缺 tenant_id 返回 400（文档既有承诺）。
func TestSearchMissingTenantRejected(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	seedSearchCorpus(t, b)

	code, body := searchGET(t, srv, url.Values{"q": {"ERP"}, "viewer": {"u1"}})
	if code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", code, body)
	}
	if !strings.Contains(body, "tenant_id required") {
		t.Fatalf("want error 'tenant_id required', got %s", body)
	}
}

// TestSearchTenantIsolation 相同文本在不同租户下互不可见。
func TestSearchTenantIsolation(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	putSearchMemory(t, b, "m-t1", "t1", "u1", VisTenant, KindFact, "共享会议纪要模板")
	putSearchMemory(t, b, "m-t2", "t2", "u2", VisTenant, KindFact, "共享会议纪要模板")

	onlyID(t, searchIDs(t, srv, url.Values{"q": {"会议纪要"}, "tenant_id": {"t1"}, "viewer": {"u1"}}), "m-t1")
	onlyID(t, searchIDs(t, srv, url.Values{"text": {"会议纪要"}, "tenant_id": {"t2"}, "viewer": {"u2"}}), "m-t2")
}

// TestSearchViewerPrivateIsolation 私有记忆对同租户的其他 viewer 不可见。
func TestSearchViewerPrivateIsolation(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	putSearchMemory(t, b, "m-priv", "t1", "u2", VisPrivate, KindFact, "u2 的私密备注：预算表")

	if ids := searchIDs(t, srv, url.Values{"q": {"私密备注"}, "tenant_id": {"t1"}, "viewer": {"u1"}}); len(ids) != 0 {
		t.Fatalf("private memory must be invisible to u1, got %v", ids)
	}
	onlyID(t, searchIDs(t, srv, url.Values{"q": {"私密备注"}, "tenant_id": {"t1"}, "viewer": {"u2"}}), "m-priv")
}

// TestSearchKindFilter kind 过滤仍然生效（未被参数改动破坏）。
func TestSearchKindFilter(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	putSearchMemory(t, b, "m-fact", "t1", "u1", VisTenant, KindFact, "客户A偏好周一开会")
	putSearchMemory(t, b, "m-pref", "t1", "u1", VisTenant, KindPreference, "客户A偏好上午沟通")

	onlyID(t, searchIDs(t, srv, url.Values{
		"q": {"客户A"}, "kind": {"preference"}, "tenant_id": {"t1"}, "viewer": {"u1"},
	}), "m-pref")

	ids := searchIDs(t, srv, url.Values{"q": {"客户A"}, "tenant_id": {"t1"}, "viewer": {"u1"}})
	if len(ids) != 2 {
		t.Fatalf("without kind filter want 2 hits, got %v", ids)
	}
}

// TestSearchTopK top_k 截断生效，缺省默认 10。
func TestSearchTopK(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	for i := 0; i < 3; i++ {
		putSearchMemory(t, b, fmt.Sprintf("m-x%d", i), "t1", "u1", VisTenant, KindFact,
			fmt.Sprintf("项目X的第 %d 条记录", i))
	}

	if ids := searchIDs(t, srv, url.Values{"q": {"项目X"}, "top_k": {"2"}, "tenant_id": {"t1"}, "viewer": {"u1"}}); len(ids) != 2 {
		t.Fatalf("top_k=2 want 2 hits, got %v", ids)
	}
	if ids := searchIDs(t, srv, url.Values{"q": {"项目X"}, "tenant_id": {"t1"}, "viewer": {"u1"}}); len(ids) != 3 {
		t.Fatalf("default top_k want 3 hits, got %v", ids)
	}
}

// TestSearchCaseInsensitive 文本匹配大小写不敏感（后端既有语义，回归守护）。
func TestSearchCaseInsensitive(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	seedSearchCorpus(t, b)

	onlyID(t, searchIDs(t, srv, url.Values{"q": {"erp"}, "tenant_id": {"t1"}, "viewer": {"u1"}}), "m-erp")
}

// TestSearchWhitespaceTrimmed 查询串两端空白被裁剪，仍能命中。
func TestSearchWhitespaceTrimmed(t *testing.T) {
	srv, b := newSearchTestEnv(t)
	seedSearchCorpus(t, b)

	onlyID(t, searchIDs(t, srv, url.Values{"q": {"  ERP  "}, "tenant_id": {"t1"}, "viewer": {"u1"}}), "m-erp")
}
