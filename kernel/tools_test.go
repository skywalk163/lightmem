// Agent 记忆工具端点单测（任务书 §4.4）。
package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 用 httptest 跑端到端 HTTP 单测。
func newTestServer(b StorageBackend) *APIServer {
	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, b)
	srv.SetRankFusion(DefaultRankFusion())
	return srv
}

func doJSON(srv *APIServer, method, path string, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&b).Encode(body)
	}
	req := httptest.NewRequest(method, path, &b)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestToolRecall(t *testing.T) {
	b, _ := newSQLiteBackend(context.Background(), nil)
	srv := newTestServer(b)

	_ = b.Upsert(context.Background(), &Memory{
		MemoryID: "r1", IdempotencyKey: "r1", TenantID: "t1", UserID: "u1",
		Visibility: VisTenant, MemoryKind: KindFact, Fact: "客户A偏好周二开会",
		Confidence: 0.9, Layer: LayerL1,
		Permission: Permission{Owner: "u1"},
	})

	w := doJSON(srv, "POST", "/tools/memory/recall", map[string]any{
		"text":      "客户A 偏好",
		"tenant_id": "t1",
		"user_id":   "u1",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("recall: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Intent string        `json:"intent"`
		Fused  []interface{} `json:"fused"`
		Mode   string        `json:"mode"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if len(res.Fused) == 0 {
		t.Fatalf("recall should return fused results, got %+v", res)
	}
	if res.Intent != "query_knowledge" {
		t.Fatalf("want intent=query_knowledge, got %s", res.Intent)
	}
}

func TestToolSave(t *testing.T) {
	b, _ := newSQLiteBackend(context.Background(), nil)
	srv := newTestServer(b)

	w := doJSON(srv, "POST", "/tools/memory/save", map[string]any{
		"memory_id":       "s1",
		"idempotency_key": "k1",
		"tenant_id":       "t1",
		"user_id":         "u1",
		"memory_kind":     "fact",
		"fact":            "客户A偏好周二开会",
		"confidence":      0.9,
		"visibility":      "tenant",
		"permission":      map[string]interface{}{"owner": "u1"},
		"source":          "chat",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("save: want 201, got %d: %s", w.Code, w.Body.String())
	}

	// 幂等：同 idempotency_key 应返回 200 + idempotent_noop。
	w2 := doJSON(srv, "POST", "/tools/memory/save", map[string]any{
		"memory_id":       "s2",
		"idempotency_key": "k1",
		"tenant_id":       "t1",
		"user_id":         "u1",
		"memory_kind":     "fact",
		"fact":            "客户A偏好周三开会",
		"confidence":      0.9,
		"visibility":      "tenant",
		"permission":      map[string]interface{}{"owner": "u1"},
		"source":          "chat",
	})
	if w2.Code != http.StatusOK {
		t.Fatalf("idempotent save: want 200, got %d: %s", w2.Code, w2.Body.String())
	}
	var res struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &res)
	if res.Status != "idempotent_noop" {
		t.Fatalf("want status=idempotent_noop, got %s", res.Status)
	}
}

func TestToolUpdate(t *testing.T) {
	b, _ := newSQLiteBackend(context.Background(), nil)
	srv := newTestServer(b)

	_ = b.Upsert(context.Background(), &Memory{
		MemoryID: "u1", IdempotencyKey: "u1", TenantID: "t1", UserID: "u1",
		Visibility: VisTenant, MemoryKind: KindFact, Fact: "原事实",
		Version: 1, Layer: LayerL1, RecordedAt: time.Now().UTC().Add(-1 * time.Hour),
		Permission: Permission{Owner: "u1"},
	})

	w := doJSON(srv, "POST", "/tools/memory/update", map[string]any{
		"memory_id": "u1",
		"fact":      "新事实",
		"viewer":    "u1",
		"user_id":   "u1",
		"tenant_id": "t1",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("update: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Status     string `json:"status"`
		Superseded string `json:"superseded"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Status != "updated" {
		t.Fatalf("want status=updated, got %s", res.Status)
	}
	if res.Superseded != "u1" {
		t.Fatalf("want superseded=u1, got %s", res.Superseded)
	}

	// 旧版本应被标记 superseded_by。
	old, _ := b.Get(context.Background(), "u1", "u1", "t1")
	if old.SupersededBy == nil {
		t.Fatalf("old version should be superseded")
	}
}

func TestToolFeedback(t *testing.T) {
	b, _ := newSQLiteBackend(context.Background(), nil)
	srv := newTestServer(b)

	_ = b.Upsert(context.Background(), &Memory{
		MemoryID: "f1", IdempotencyKey: "f1", TenantID: "t1", UserID: "u1",
		Visibility: VisTenant, MemoryKind: KindFact, Fact: "客户A偏好周二开会",
		Confidence: 0.9, Layer: LayerL1, RecordedAt: time.Now().UTC().Add(-1 * time.Hour),
		Permission: Permission{Owner: "u1"},
	})

	w := doJSON(srv, "POST", "/tools/memory/feedback", map[string]any{
		"memory_id": "f1",
		"status":    "rejected",
		"operator":  "u1",
		"viewer":    "u1",
		"user_id":   "u1",
		"tenant_id": "t1",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("feedback: want 200, got %d: %s", w.Code, w.Body.String())
	}

	m, _ := b.Get(context.Background(), "f1", "u1", "t1")
	if len(m.FeedbackRecords) != 1 || m.FeedbackRecords[0].Status != FbRejected {
		t.Fatalf("feedback record not recorded: %+v", m.FeedbackRecords)
	}
	// rejected 应降权。
	if m.Confidence >= 0.9 {
		t.Fatalf("rejected should lower confidence, got %.2f", m.Confidence)
	}
}

func TestToolForget(t *testing.T) {
	b, _ := newSQLiteBackend(context.Background(), nil)
	srv := newTestServer(b)

	_ = b.Upsert(context.Background(), &Memory{
		MemoryID: "d1", IdempotencyKey: "d1", TenantID: "t1", UserID: "u1",
		Visibility: VisTenant, MemoryKind: KindFact, Fact: "客户A偏好周二开会",
		Confidence: 0.9, Layer: LayerL1, RecordedAt: time.Now().UTC().Add(-1 * time.Hour),
		Permission: Permission{Owner: "u1"},
	})

	w := doJSON(srv, "POST", "/tools/memory/forget", map[string]any{
		"memory_id": "d1",
		"reason":    "用户请求删除",
		"viewer":    "u1",
		"user_id":   "u1",
		"tenant_id": "t1",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("forget: want 200, got %d: %s", w.Code, w.Body.String())
	}

	m, _ := b.Get(context.Background(), "d1", "u1", "t1")
	if !m.IsHistorical {
		t.Fatalf("forget should set is_historical=true")
	}
	if m.SupersededBy == nil || *m.SupersededBy != "forgotten:用户请求删除" {
		t.Fatalf("forget should set superseded_by, got %+v", m.SupersededBy)
	}
}

// 端到端：RedactionPolicy 经 /tools/memory/save 生效。
func TestToolSaveRedaction(t *testing.T) {
	b, _ := newSQLiteBackend(context.Background(), nil)
	srv := newTestServer(b)

	w := doJSON(srv, "POST", "/tools/memory/save", map[string]any{
		"memory_id":       "r1",
		"idempotency_key": "r1",
		"tenant_id":       "t1",
		"user_id":         "u1",
		"memory_kind":     "fact",
		"fact":            "联系人邮箱 a.b@corp.com",
		"confidence":      0.9,
		"visibility":      "tenant",
		"permission":      map[string]interface{}{"owner": "u1"},
		"source":          "chat",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("save: want 201, got %d: %s", w.Code, w.Body.String())
	}

	m, _ := b.Get(context.Background(), "r1", "u1", "t1")
	if m.PermissionLevel != PermissionLevelConfidential {
		t.Fatalf("redaction should set permission_level=%s, got %s", PermissionLevelConfidential, m.PermissionLevel)
	}
}
