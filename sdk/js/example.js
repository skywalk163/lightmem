#!/usr/bin/env node
// LightMem JS SDK CRUD 示例。
//
// 演示：创建 → 检索 → 更新 → 导出 → 被遗忘（删除）完整闭环。
// 需先启动 kernel 服务（sqlite 骨架）：
//   lightmem-kernel -backend-override sqlite -listen :8080
//
// 运行：
//   node example.js [base_url]

import { LightMemClient } from "./lightmem-sdk.js";

const base = process.argv[2] || "http://localhost:8080";
const client = new LightMemClient(base);

const tenant = "demo-tenant";
const user = "demo-user";

function assert(cond, msg) {
  if (!cond) {
    console.error("ASSERT FAILED:", msg);
    process.exit(1);
  }
}

async function main() {
  console.log("== LightMem JS SDK CRUD 示例 (base=" + base + ") ==");

  // 1) Create
  let r = await client.save({
    tenant_id: tenant,
    user_id: user,
    memory_kind: "fact",
    fact: "用户 skywalk 偏好用中文交流",
    visibility: "private",
    confidence: 0.9,
    permission: { owner: user },
  });
  assert([200, 201].includes(r.status), "save failed: " + JSON.stringify(r));
  const mid = r.body.memory_id || (r.body.memory && r.body.memory.memory_id);
  console.log("[C] create -> status=" + r.status + " memory_id=" + mid);
  assert(mid, "memory_id missing");

  // 2) Read (search)
  r = await client.search("中文交流", tenant, { viewer: user });
  assert(r.status === 200, "search failed: " + JSON.stringify(r));
  const hits = Array.isArray(r.body) ? r.body : (r.body.memories || []);
  console.log("[R] search -> status=" + r.status + " hits=" + hits.length);
  assert(hits.length >= 1, "search returned no hits");

  // 2b) Read (recall + fuse)
  r = await client.recall({ text: "中文交流", tenant_id: tenant, user_id: user });
  assert(r.status === 200, "recall failed: " + JSON.stringify(r));
  console.log("[R] recall+fuse -> status=" + r.status + " fused=" + (r.body.fused || []).length);

  // 3) Update
  r = await client.update({ memoryId: mid, fact: "用户 skywalk 偏好用中文与英文混合交流", tenantId: tenant, viewer: user, userId: user });
  assert(r.status === 200, "update failed: " + JSON.stringify(r));
  console.log("[U] update -> status=" + r.status + " new_id=" + (r.body.memory_id || (r.body.memory && r.body.memory.memory_id)));

  // 检索确认更新生效
  r = await client.search("英文混合", tenant, { viewer: user });
  const hits2 = Array.isArray(r.body) ? r.body : (r.body.memories || []);
  console.log("[R] search-after-update -> status=" + r.status + " hits=" + hits2.length);
  assert(hits2.length >= 1, "updated fact not searchable");

  // 4) Export (GDPR §7.4)
  r = await client.exportAll(tenant, user);
  assert(r.status === 200, "export failed: " + JSON.stringify(r));
  console.log("[GDPR] export -> status=" + r.status + " exported=" + r.body.exported);

  // 5) Delete (GDPR §7.5 被遗忘权)
  r = await client.forget(mid, user);
  assert(r.status === 200, "forget failed: " + JSON.stringify(r));
  console.log("[D] forget -> status=" + r.status + " result=" + JSON.stringify(r.body));

  // 删除后检索应不再命中
  r = await client.search("中文交流", tenant, { viewer: user });
  const hits3 = Array.isArray(r.body) ? r.body : (r.body.memories || []);
  console.log("[R] search-after-forget -> status=" + r.status + " hits=" + hits3.length);

  console.log("\n== CRUD + GDPR 闭环全部跑通 ==");
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
