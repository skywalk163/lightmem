// R24-T0 判据 5：接缝预埋零回归**对拍**工具。
//
// 任务书 §3.2 预埋红线要求：`routeHooks` 为空时 `Handler()` 输出必须与 `c908091`
// **逐字节一致**。本文件用 httptest 起服务，把 `/health`、`POST /memories`、
// `GET /search` 三端点的响应原样落盘；把它原样复制到 `c908091` 的干净工作树里
// 跑一遍，两份输出 diff 即为该判据的硬证据。
//
// 本文件是**自包含**的：只依赖 R23.3（c908091）已有的符号，不引用任何 R24 新增
// 类型，因此可在干净基线上编译运行。
//
// 用法（两棵树各跑一次）：
//
//	cd <tree>/kernel && R24_SEAM_DUMP=<out.json> go test -count=1 -run TestDumpR24SeamHTTP .
//	cmp <base.json> <cur.json>   # 空输出 = 逐字节一致
package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// seamClockFields 需归一化的时钟字段：服务端用 time.Now 落库，跨进程必然不同，
// 属「时钟」非「行为」，对拍前必须抹平（与 lookup_diffdump_test.go 同口径）。
var seamClockFields = []string{"recorded_at", "updated_at", "timestamp", "created_at", "last_accessed_at"}

// seamClockRegexps 预编译时钟字段正则（形如 "recorded_at":"2026-..."）。
var seamClockRegexps = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(seamClockFields))
	for _, f := range seamClockFields {
		out = append(out, regexp.MustCompile(`"`+f+`"\s*:\s*"[^"]*"`))
	}
	return out
}()

// normalizeSeamClocks 把**原始响应字符串**里的时钟字段替换为固定占位符。
//
// 必须在拼进 recs（进而被 json.Marshal 转义成 \"）**之前**调用：
// 一旦嵌套进 JSON 字符串，双引号变成 \"，正则就匹配不到了——第一轮对拍就是
// 栽在这里（差异全是 recorded_at，但归一化没生效）。
func normalizeSeamClocks(s string) string {
	for i, re := range seamClockRegexps {
		s = re.ReplaceAllString(s, `"`+seamClockFields[i]+`":"FIXED"`)
	}
	return s
}

// seamUpsert 一条固定记忆（POST /memories），返回原始响应体。
func seamUpsert(t *testing.T, base string, id string, tenant, user string, vis Visibility, fact string) string {
	t.Helper()
	body := map[string]any{
		"memory_id":       id,
		"idempotency_key": id,
		"tenant_id":       tenant,
		"user_id":         user,
		"visibility":      string(vis),
		"memory_kind":     "fact",
		"fact":            fact,
		"confidence":      0.9,
		"source":          "r24-t0-seam",
		"importance":      0.5,
		"permission":      map[string]any{"owner": user},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal upsert body: %v", err)
	}
	resp, err := http.Post(base+"/memories", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post /memories: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /memories: %v", err)
	}
	return fmt.Sprintf("%d|%s", resp.StatusCode, normalizeSeamClocks(strings.TrimSpace(string(b))))
}

// seamGet 发一个 GET 请求，返回 "状态码|响应体"（时钟字段已归一化）。
func seamGet(t *testing.T, base, path string) string {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return fmt.Sprintf("%d|%s", resp.StatusCode, normalizeSeamClocks(strings.TrimSpace(string(b))))
}

// TestDumpR24SeamHTTP 落盘三端点响应（判据 5 的跨树对拍证据）。
//
// 覆盖：
//   - GET /health：无数据依赖，最先验证路由表本身没被接缝改动；
//   - POST /memories：写路径（含 201 与幂等 noop 两种响应）；
//   - GET /search：读路径（命中 / 空集 / 缺 tenant_id 的 400 / 缺 q 的 400）。
//
// 确定性：语料与查询全部固定；时钟字段归一化；map 型 JSON 由 encoding/json
// 按 key 排序输出，struct 型按字段定义序，均为确定性编码。
func TestDumpR24SeamHTTP(t *testing.T) {
	out := os.Getenv("R24_SEAM_DUMP")
	if out == "" {
		t.Skip("未设置 R24_SEAM_DUMP，跳过落盘（差分测试需显式指定输出路径）")
	}

	b, err := newSQLiteBackend(context.Background(), nil)
	if err != nil {
		t.Fatalf("newSQLiteBackend: %v", err)
	}
	srv := NewAPIServer(&Profile{Profile: TierSolo, StorageBackend: BackendSQLite}, b)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	base := ts.URL

	recs := []string{}

	// 1) /health —— 路由表基线
	recs = append(recs, "GET /health => "+seamGet(t, base, "/health"))

	// 2) POST /memories —— 写路径（首次 201）
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("seam-m%02d", i)
		fact := fmt.Sprintf("客户%02d的偏好是事项%02d", i, i)
		recs = append(recs, fmt.Sprintf("POST /memories %s => ", id)+
			seamUpsert(t, base, id, "t1", "u1", VisTenant, fact))
	}
	// 幂等重写（第二次应走 idempotent_noop 分支）
	recs = append(recs, "POST /memories(dup) => "+
		seamUpsert(t, base, "seam-m01", "t1", "u1", VisTenant, "客户01的偏好是事项01"))

	// 3) GET /search —— 读路径
	searches := []string{
		"/search?tenant_id=t1&q=%E5%AE%A2%E6%88%B701&viewer=u1",    // 客户01
		"/search?tenant_id=t1&q=%E5%81%8F%E5%A5%BD&viewer=u1",      // 偏好（多命中）
		"/search?tenant_id=t1&q=nonexistent&viewer=u1",             // 空集
		"/search?tenant_id=t1&text=%E4%BA%8B%E9%A1%B902&viewer=u1", // text 别名：事项02
		"/search?q=%E5%81%8F%E5%A5%BD&viewer=u1",                   // 缺 tenant_id → 400
		"/search?tenant_id=t1&viewer=u1",                           // 缺 q → 400
	}
	for _, s := range searches {
		recs = append(recs, "GET "+s+" => "+seamGet(t, base, s))
	}

	// 4) 再取一次 /health，确认写操作没改变路由表/状态输出
	recs = append(recs, "GET /health(2nd) => "+seamGet(t, base, "/health"))

	payload := map[string]any{
		"dumped_at_unix_fixed": true, // 显式声明：本 dump 不含 wall clock
		"records":              recs,
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(out, append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", out, err)
	}
	t.Logf("dumped %d endpoint records -> %s (%d bytes)", len(recs), out, len(raw)+1)
}
