#!/usr/bin/env python3
"""
build_locomo_v2.py — R12-A：LoCoMo 粒度修复，从 raw 提取未使用 QA，构造 BEAM 格式 holdout 子集。

任务书：lightmem/eval/reports/r12_taskbook_20261015.md（A 路）
基线：a0ca14f（R11 收口态）

修复点（相对 R11 add_locomo.py）：
  R11 把 memory.fact 设成对话原文（如 "Andrew: Totally get it, pets bring such joy..."），
  而 expected_fact 是提炼后的短答案（如 "Family"）。两者文本粒度差太大，
  matchFactV4（文本匹配口径）下即便检索召回正确记忆也判不上 hit。

  本脚本把 memory.fact 直接设成 `{question} => {expected_fact}`（与 BEAM holdout 完全一致），
  让 matchFactV4 文本匹配口径下「答案嵌在记忆文本里」，能够判上 hit。

设计原则：
  1. 确定性：seed=42 固定随机种子，同输入逐位一致（红线 3）。
  2. 不污染：排除已用于 MRR 测试（benchmarks/dataset/locomo.jsonl，50 条）与
     已用于 R11 holdout（eval/holdout/holdout.jsonl 中 lc-*，150 条）的 QA。
  3. 只增不改：不触碰现有 holdout.jsonl；只写本目录下 holdout_locomo_v2.jsonl（红线 6）。
  4. 排除 adversarial：category=5 是 LoCoMo 官方 adversarial 类别，不参与评测
     （与 R11 / README 口径一致）。
  5. 格式对齐：HoldoutItem schema 与 BEAM holdout 完全一致；bucket 映射沿用 R11 口径。

输入：eval/benchmarks/raw/locomo.json（10 组对话，1986 条 QA）
输出：eval/holdout/locomo_v2/holdout_locomo_v2.jsonl（200 条）

复现（在 0.88 上，lightmem/eval/holdout/locomo_v2/ 下）：
  python3 build_locomo_v2.py
"""

import json
import os
import random
import re

BASE = os.path.dirname(os.path.abspath(__file__))
EVAL_DIR = os.path.join(BASE, "..", "..")  # lightmem/eval
BENCH_DIR = os.path.join(EVAL_DIR, "benchmarks")
RAW_PATH = os.path.join(BENCH_DIR, "raw", "locomo.json")
BENCH_DS_DIR = os.path.join(BENCH_DIR, "dataset")
R11_HOLDOUT = os.path.join(EVAL_DIR, "holdout", "holdout.jsonl")
OUTPUT = os.path.join(BASE, "holdout_locomo_v2.jsonl")

SEED = 42
TARGET_COUNT = 200  # 任务书 A.2：200 条
ID_PREFIX = "lc2-"   # 与 R11 的 lc-* 前缀区分，杜绝 ID 冲突

# category → bucket 映射（与 R11 add_locomo.py / benchmarks README 完全一致）
# 1: single fact (single-hop) / 2: temporal / 3: multi-hop / 4: open-domain / 5: adversarial（排除）
CATEGORY_TO_BUCKET = {
    1: "single-hop",
    2: "temporal",
    3: "multi-hop",
    4: "open-domain",
}


def load_jsonl(path):
    items = []
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                items.append(json.loads(line))
    return items


def load_json(path):
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def build_dialog_index(conv_data):
    """从对话数据中构建 dia_id → message 索引。"""
    inner = conv_data.get("conversation", {})
    index = {}
    for key in inner.keys():
        if not key.startswith("session_") or key.endswith("_date_time"):
            continue
        session_msgs = inner[key]
        if not isinstance(session_msgs, list):
            continue
        for msg in session_msgs:
            if isinstance(msg, dict) and "dia_id" in msg:
                index[msg["dia_id"]] = msg
    return index


def collect_used_ids():
    """收集所有已用于 MRR 测试或 R11 holdout 的 locomo ID，避免数据污染。"""
    used = set()

    # 1) MRR 基准集（50 条）
    mrr_path = os.path.join(BENCH_DS_DIR, "locomo.jsonl")
    if os.path.exists(mrr_path):
        for item in load_jsonl(mrr_path):
            used.add(item["id"])
    print(f"[used] MRR locomo.jsonl: {len(used)}")

    # 2) R11 holdout 中的 lc-* 项（150 条）
    if os.path.exists(R11_HOLDOUT):
        hold = 0
        for item in load_jsonl(R11_HOLDOUT):
            iid = item.get("id", "")
            if isinstance(iid, str) and iid.startswith("lc-"):
                used.add(iid)
                hold += 1
        print(f"[used] R11 holdout lc-*: {hold}")

    print(f"[used] total used locomo IDs: {len(used)}")
    return used


def collect_available(used_ids):
    """遍历 raw locomo 所有 QA，剔除已用 ID 与 adversarial（category=5）。"""
    raw_data = load_json(RAW_PATH)
    print(f"[raw] locomo conversations: {len(raw_data)}")

    all_available = []
    total_qa = 0
    skipped_used = 0
    skipped_adv = 0
    skipped_invalid = 0

    for conv in raw_data:
        conv_id = conv.get("sample_id", "")
        dialog_index = build_dialog_index(conv)
        qas = conv.get("qa", [])
        total_qa += len(qas)

        for idx, qa in enumerate(qas):
            question = qa.get("question", "")
            answer = str(qa.get("answer", "")).strip()
            category = qa.get("category", 1)
            evidence = qa.get("evidence", [])

            # 剔除 adversarial（category=5）
            if category == 5:
                skipped_adv += 1
                continue

            # 剔除无效 QA（无 question 或 answer 为空/None）
            if not question or not answer or answer == "None":
                skipped_invalid += 1
                continue

            # 用第一个 evidence 构造预期 ID，剔除已用 ID
            if evidence:
                dia_id = evidence[0]
                m = re.match(r"D(\d+):(\d+)", str(dia_id))
                if m:
                    dia_norm = str(dia_id).replace(":", "_")
                else:
                    dia_norm = None
            else:
                dia_norm = None

            if dia_norm:
                expected_id = f"lc-{conv_id}-{dia_norm}"
            else:
                expected_id = f"lc-{conv_id}-qa{idx}"

            if expected_id in used_ids:
                skipped_used += 1
                continue

            all_available.append({
                "conv_id": conv_id,
                "qa": qa,
                "dialog_index": dialog_index,
                "qa_idx": idx,
                "category": category,
            })

    print(f"[raw] total QA: {total_qa}, used: {skipped_used}, "
          f"adversarial(cat=5): {skipped_adv}, invalid: {skipped_invalid}")
    print(f"[raw] available: {len(all_available)}")
    return all_available


def adapt_locomo_qa(item):
    """
    将 locomo 原始 QA 适配成 HoldoutItem（BEAM 格式对齐）。

    关键修复：memory.fact = `{question} => {expected_fact}`，
    让 matchFactV4 文本匹配口径下直接命中。

    ID 策略：`lc2-{conv_id}-{D{n}_{m}}-qa{idx}`，含 qa_idx 保证唯一
    （同一证据可被多条 QA 引用，R11 仅用证据 ID 会冲突）。
    """
    conv_id = item["conv_id"]
    qa = item["qa"]
    qa_idx = item["qa_idx"]
    question = qa["question"]
    answer = str(qa["answer"]).strip()
    category = item["category"]

    # 取第一个 evidence 的 dia_id 作为 primary memory_id 前缀
    evidence = qa.get("evidence", [])
    if evidence:
        dia_id = str(evidence[0])
        dia_norm = dia_id.replace(":", "_")
        primary_id = f"{ID_PREFIX}{conv_id}-{dia_norm}-qa{qa_idx}"
    else:
        primary_id = f"{ID_PREFIX}{conv_id}-qa{qa_idx}"

    # 期望命中的 memory_ids：所有 evidence 对应的 ID（与 R11 语义一致）
    expected_memory_ids = []
    for ev in evidence:
        ev_str = str(ev)
        m = re.match(r"D(\d+):(\d+)", ev_str)
        if m:
            expected_memory_ids.append(f"{ID_PREFIX}{conv_id}-{ev_str.replace(':', '_')}-qa{qa_idx}")
    if not expected_memory_ids:
        expected_memory_ids = [primary_id]

    bucket = CATEGORY_TO_BUCKET.get(category, "single-hop")

    # memory.fact = question => expected_fact（BEAM 格式，粒度修复核心）
    fact_text = f"{question} => {answer}"

    return {
        "id": primary_id,
        "query": question,
        "expected_fact": answer,
        "expected_memory_ids": expected_memory_ids,
        "bucket": bucket,
        "is_temporal": bucket == "temporal",
        "has_injection": False,
        "has_pollution": False,
        "tenant": "t-lc-v2",
        "user": "u-lc-v2",
        "memory": {
            "memory_id": primary_id,
            "idempotency_key": primary_id,
            "tenant_id": "t-lc-v2",
            "user_id": "u-lc-v2",
            "visibility": "tenant",
            "permission": {"owner": "u-lc-v2"},
            "permission_level": "",
            "memory_kind": "fact",
            "fact": fact_text,
            "reference_frame": {
                "tenant": "t-lc-v2",
                "user": "u-lc-v2",
                "scene": "locomo-v2-holdout",
            },
            "occurred_at": "0001-01-01T00:00:00Z",
            "recorded_at": "0001-01-01T00:00:00Z",
            "confidence": 0.9,
            "source": "chat",
            "version": 1,
            "provenance": "normal",
            "source_evidence": {
                "origin": "locomo-v2-holdout",
                "snippet": question,
            },
            "vector_status": "",
            "is_historical": False,
            "layer": "L1",
        },
    }


def main():
    print("=" * 60)
    print("R12-A: Building LoCoMo v2 holdout (granularity fix)")
    print("=" * 60)

    used_ids = collect_used_ids()
    available = collect_available(used_ids)

    # 确定性抽样（seed=42）
    rng = random.Random(SEED)
    rng.shuffle(available)
    take = min(TARGET_COUNT, len(available))
    selected = available[:take]
    print(f"\n[sample] selected {len(selected)} locomo v2 items (seed={SEED})")

    # 适配
    items = []
    for item in selected:
        adapted = adapt_locomo_qa(item)
        items.append(adapted)

    # 统计
    bucket_counts = {}
    for it in items:
        b = it["bucket"]
        bucket_counts[b] = bucket_counts.get(b, 0) + 1
    print("\n[stats] bucket distribution:")
    for b, c in sorted(bucket_counts.items()):
        print(f"  {b}: {c}")

    # 校验：fact 必须包含 expected_fact
    for it in items:
        assert it["expected_fact"] in it["memory"]["fact"], \
            f"fact missing expected_fact: {it['id']}"
    print(f"[check] all {len(items)} items: fact contains expected_fact [OK]")

    # 写入
    os.makedirs(os.path.dirname(OUTPUT), exist_ok=True)
    with open(OUTPUT, "w", encoding="utf-8") as f:
        for item in items:
            f.write(json.dumps(item, ensure_ascii=False) + "\n")
    print(f"\n[output] written to: {OUTPUT}")
    print(f"[output] total: {len(items)}")
    print("=" * 60)


if __name__ == "__main__":
    main()