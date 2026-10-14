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
//
// 版本历史：
//   0.1.0 — 基础版（R5-C 交付，无错误处理/超时）
//   0.2.0 — 生产级增强（错误处理 / 超时 / JSDoc）

/** @typedef {Object} LightMemResponse
  @property {number} status - HTTP 状态码
  @property {Object|null} body - 响应体
*/

/**
 * LightMem 客户端错误（含 HTTP 状态码）。
 */
export class LightMemError extends Error {
  /**
   * @param {string} message
   * @param {number} [statusCode]
   */
  constructor(message, statusCode) {
    super(message);
    this.name = 'LightMemError';
    /** @type {number} */
    this.statusCode = statusCode || 0;
  }
}

/**
 * 连接 / 超时错误。
 */
export class LightMemConnectionError extends LightMemError {
  constructor(message) {
    super(message, 0);
    this.name = 'LightMemConnectionError';
  }
}

/**
 * API 服务端错误（4xx / 5xx）。
 */
export class LightMemAPIError extends LightMemError {
  /**
   * @param {string} message
   * @param {number} statusCode
   * @param {Object|null} [body]
   */
  constructor(message, statusCode, body) {
    super(message, statusCode);
    this.name = 'LightMemAPIError';
    /** @type {Object|null} */
    this.body = body || null;
  }
}

/**
 * 404 资源未找到。
 */
export class LightMemNotFoundError extends LightMemAPIError {
  /**
   * @param {string} [message]
   * @param {Object|null} [body]
   */
  constructor(message = 'resource not found', body) {
    super(message, 404, body);
    this.name = 'LightMemNotFoundError';
  }
}

/**
 * LightMem REST API 客户端。
 *
 * @example
 * const c = new LightMemClient('http://localhost:8080', { timeoutMs: 15000 });
 * const { status, body } = await c.save({ tenant_id: 't1', user_id: 'u1', fact: 'hello' });
 */
export class LightMemClient {
  /**
   * @param {string} [baseUrl='http://localhost:8080'] - 服务地址
   * @param {Object} [opts] - 选项
   * @param {number} [opts.timeoutMs=10000] - 请求超时（毫秒）
   */
  constructor(baseUrl = 'http://localhost:8080', { timeoutMs = 10000 } = {}) {
    /** @type {string} */
    this.baseUrl = baseUrl.replace(/\/$/, '');
    /** @type {number} */
    this.timeoutMs = timeoutMs;
  }

  /**
   * 底层请求方法。
   *
   * @param {string} method - HTTP 方法
   * @param {string} path - 请求路径（含 query）
   * @param {Object|null} [payload] - 请求体
   * @returns {Promise<LightMemResponse>}
   * @throws {LightMemConnectionError|LightMemAPIError|LightMemNotFoundError}
   */
  async _request(method, path, payload) {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), this.timeoutMs);
    try {
      const opts = {
        method,
        headers: { 'Content-Type': 'application/json; charset=utf-8' },
        signal: ctrl.signal,
      };
      if (payload !== undefined) {
        opts.body = JSON.stringify(payload);
      }
      const resp = await fetch(this.baseUrl + path, opts);
      let body = null;
      const text = await resp.text();
      if (text) {
        try { body = JSON.parse(text); } catch { body = { raw: text }; }
      }
      if (resp.status === 404) {
        throw new LightMemNotFoundError(body?.error || 'not found', body);
      }
      if (resp.status >= 400) {
        throw new LightMemAPIError(
          body?.error || `HTTP ${resp.status}`,
          resp.status,
          body
        );
      }
      return { status: resp.status, body };
    } catch (err) {
      if (err instanceof LightMemError) throw err;
      if (err.name === 'AbortError') {
        throw new LightMemConnectionError(`request timeout (${this.timeoutMs}ms)`);
      }
      throw new LightMemConnectionError(err.message || String(err));
    } finally {
      clearTimeout(timer);
    }
  }

  // ---- CRUD ----

  /**
   * 创建 / 幂等写入一条记忆。
   *
   * @param {Object} memory - 记忆对象，至少含 tenant_id / user_id / fact
   * @returns {Promise<LightMemResponse>}
   */
  save(memory) {
    return this._request('POST', '/memories', memory);
  }

  /**
   * GET /search 文本检索。
   *
   * @param {string} q - 查询文本
   * @param {string} tenantId - 租户 ID
   * @param {Object} [opts] - 选项
   * @param {string} [opts.viewer] - 可见性过滤
   * @param {number} [opts.topK=10] - 返回条数
   * @param {string} [opts.kind] - 记忆类型过滤
   * @returns {Promise<LightMemResponse>}
   */
  search(q, tenantId, { viewer, topK = 10, kind } = {}) {
    const params = new URLSearchParams({
      q,
      tenant_id: tenantId,
      top_k: String(topK),
    });
    if (viewer) params.set('viewer', viewer);
    if (kind) params.set('kind', kind);
    return this._request('GET', '/search?' + params.toString());
  }

  /**
   * POST /tools/memory/recall 跨层召回 + 融合。
   *
   * @param {Object} rc - RouterContext 对象，至少含 text / tenant_id
   * @returns {Promise<LightMemResponse>}
   */
  recall(rc) {
    return this._request('POST', '/tools/memory/recall', rc);
  }

  /**
   * POST /tools/memory/update 编辑（追加新版本）。
   *
   * @param {Object} params - 参数
   * @param {string} params.memoryId - 记忆 ID
   * @param {string} params.fact - 新事实文本
   * @param {string} params.tenantId - 租户 ID
   * @param {string} [params.viewer] - 可见性过滤
   * @param {string} [params.userId] - 用户 ID
   * @param {number} [params.confidence] - 置信度
   * @returns {Promise<LightMemResponse>}
   */
  update({ memoryId, fact, tenantId, viewer, userId, confidence }) {
    const payload = { memory_id: memoryId, fact, tenant_id: tenantId };
    if (viewer) payload.viewer = viewer;
    if (userId) payload.user_id = userId;
    if (confidence) payload.confidence = confidence;
    return this._request('POST', '/tools/memory/update', payload);
  }

  // ---- GDPR 合规（P7-A）----

  /**
   * §7.4 数据可携导出。
   *
   * @param {string} tenantId - 租户 ID
   * @param {string} userId - 用户 ID
   * @returns {Promise<LightMemResponse>}
   */
  exportAll(tenantId, userId) {
    return this._request('POST', '/api/v1/gdpr/export', {
      tenant_id: tenantId,
      user_id: userId,
    });
  }

  /**
   * §7.3 级联删除。
   *
   * @param {string} tenantId - 租户 ID
   * @param {string} userId - 用户 ID
   * @param {string} [actor='sdk'] - 操作者标识
   * @returns {Promise<LightMemResponse>}
   */
  cascadeDelete(tenantId, userId, actor = 'sdk') {
    return this._request('POST', '/api/v1/gdpr/cascade-delete', {
      tenant_id: tenantId,
      user_id: userId,
      actor,
    });
  }

  /**
   * §7.5 被遗忘权。
   *
   * @param {string} memoryId - 记忆 ID
   * @param {string} [actor='sdk'] - 操作者标识
   * @returns {Promise<LightMemResponse>}
   */
  forget(memoryId, actor = 'sdk') {
    return this._request('POST', '/api/v1/gdpr/forget', {
      memory_id: memoryId,
      actor,
    });
  }
}

// CommonJS 兼容（Node < 14 或 require 场景）
if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    LightMemClient,
    LightMemError,
    LightMemConnectionError,
    LightMemAPIError,
    LightMemNotFoundError,
  };
}
