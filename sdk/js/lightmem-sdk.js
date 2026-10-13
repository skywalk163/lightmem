// LightMem REST SDK（JavaScript / Node，依赖内置 fetch，Node 18+）。
//
// 对应 kernel 的 HTTP API 骨架（§4.4）。覆盖 CRUD + 检索 + GDPR 合规端点。
// 仅用全局 fetch，无第三方依赖。
//
// 端点清单（见 sdk/README.md）：
//   POST   /memories                  创建/幂等写入一条记忆
//   GET    /search?q=&tenant_id=...   文本检索
//   POST   /tools/memory/recall       跨层召回 + 多信号融合
//   POST   /tools/memory/update       编辑（追加新版本，不覆盖）
//   POST   /api/v1/gdpr/export        数据可携导出（§7.4）
//   POST   /api/v1/gdpr/cascade-delete 级联删除（§7.3）
//   POST   /api/v1/gdpr/forget        被遗忘权（§7.5）

export class LightMemClient {
  constructor(baseUrl = "http://localhost:8080", timeoutMs = 10000) {
    this.baseUrl = baseUrl.replace(/\/$/, "");
    this.timeoutMs = timeoutMs;
  }

  async _request(method, path, payload) {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), this.timeoutMs);
    try {
      const opts = {
        method,
        headers: { "Content-Type": "application/json; charset=utf-8" },
        signal: ctrl.signal,
      };
      if (payload !== undefined) opts.body = JSON.stringify(payload);
      const resp = await fetch(this.baseUrl + path, opts);
      let body = null;
      const text = await resp.text();
      if (text) {
        try { body = JSON.parse(text); } catch { body = { raw: text }; }
      }
      return { status: resp.status, body };
    } finally {
      clearTimeout(timer);
    }
  }

  // ---- CRUD ----
  /** 创建/幂等写入一条记忆 */
  save(memory) {
    return this._request("POST", "/memories", memory);
  }

  /** GET /search 文本检索 */
  search(q, tenantId, { viewer, topK = 10, kind } = {}) {
    const params = new URLSearchParams({ q, tenant_id: tenantId, top_k: String(topK) });
    if (viewer) params.set("viewer", viewer);
    if (kind) params.set("kind", kind);
    return this._request("GET", "/search?" + params.toString());
  }

  /** POST /tools/memory/recall 跨层召回 + 融合 */
  recall(rc) {
    return this._request("POST", "/tools/memory/recall", rc);
  }

  /** POST /tools/memory/update 编辑（追加新版本） */
  update({ memoryId, fact, tenantId, viewer, userId, confidence }) {
    const payload = { memory_id: memoryId, fact, tenant_id: tenantId };
    if (viewer) payload.viewer = viewer;
    if (userId) payload.user_id = userId;
    if (confidence) payload.confidence = confidence;
    return this._request("POST", "/tools/memory/update", payload);
  }

  // ---- GDPR 合规（P7-A）----
  /** §7.4 数据可携导出 */
  exportAll(tenantId, userId) {
    return this._request("POST", "/api/v1/gdpr/export", { tenant_id: tenantId, user_id: userId });
  }

  /** §7.3 级联删除 */
  cascadeDelete(tenantId, userId, actor = "sdk") {
    return this._request("POST", "/api/v1/gdpr/cascade-delete", { tenant_id: tenantId, user_id: userId, actor });
  }

  /** §7.5 被遗忘权 */
  forget(memoryId, actor = "sdk") {
    return this._request("POST", "/api/v1/gdpr/forget", { memory_id: memoryId, actor });
  }
}

// CommonJS 兼容（Node < 14 或 require 场景）
if (typeof module !== "undefined" && module.exports) {
  module.exports = { LightMemClient };
}
