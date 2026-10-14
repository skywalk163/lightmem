#!/usr/bin/env python3
"""
build_holdout.py — 从 raw benchmark 数据中切出未使用子集，构造北极星真实语料 holdout。

数据来源：
  - eval/benchmarks/raw/beam.json（700 条，已用 100 条，剩余 600 条可用）

输出：
  - eval/holdout/holdout.jsonl（~300 条，确定性 seed=42 抽样）

设计原则：
  1. 确定性：seed=42 固定随机种子，同输入逐位一致
  2. 不污染：严格排除已用于 MRR 测试的 780 条 ID
  3. 格式对齐：与 benchmarks/dataset/ 下的 HoldoutItem schema 完全一致
"""

import json
import random
import os

# ---- 路径配置 ----
BASE = os.path.dirname(os.path.abspath(__file__))
BENCH_DIR = os.path.join(BASE, "..", "benchmarks")
RAW_DIR = os.path.join(BENCH_DIR, "raw")
DATASET_DIR = os.path.join(BENCH_DIR, "dataset")
OUTPUT = os.path.join(BASE, "holdout.jsonl")

SEED = 42
TARGET_COUNT = 300  # 目标 holdout 条数


def load_jsonl(path):
    """加载 NDJSON 文件，返回 list[dict]。"""
    items = []
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                items.append(json.loads(line))
    return items


def load_json(path):
    """加载 JSON 文件。"""
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def collect_used_ids():
    """收集所有已用于 MRR 测试的 ID，避免数据污染。"""
    used = set()
    for fname in ["locomo.jsonl", "longmemeval.jsonl", "beam.jsonl"]:
        path = os.path.join(DATASET_DIR, fname)
        if os.path.exists(path):
            for item in load_jsonl(path):
                used.add(item["id"])
    print(f"[used] collected {len(used)} used IDs from MRR benchmark set")
    return used


# ---- Beam 适配 ----

# question_type → bucket 映射（从已使用的 100 条反推）
BEAM_TYPE_TO_BUCKET = {
    "preference_following": "single-hop",
    "instruction_following": "single-hop",
    "information_extraction": "multi-hop",
    "event_ordering": "temporal",
    "temporal_reasoning": "temporal",
    "knowledge_update": "multi-hop",
    "contradiction_resolution": "multi-hop",
    "summarization": "open-domain",
    "multi_session_reasoning": "multi-hop",
    "abstention": "open-domain",
}


def adapt_beam_item(raw_item):
    """将 raw beam 条目适配成 HoldoutItem 格式。"""
    qid = raw_item["question_id"]
    qtype = raw_item["question_type"]
    question = raw_item["question"]
    rubric = raw_item.get("rubric", [])
    ground_truth = raw_item.get("ground_truth_answer", "")

    # expected_fact：优先用 rubric 合并（与已使用数据一致）
    if rubric:
        expected_fact = " | ".join(rubric)
    else:
        expected_fact = ground_truth

    memory_id = f"beam-{qid}"
    fact_text = f"{question} => {expected_fact}"

    bucket = BEAM_TYPE_TO_BUCKET.get(qtype, "open-domain")

    return {
        "id": memory_id,
        "query": question,
        "expected_fact": expected_fact,
        "expected_memory_ids": [memory_id],
        "bucket": bucket,
        "is_temporal": bucket == "temporal",
        "has_injection": False,
        "has_pollution": False,
        "tenant": "t-beam",
        "user": "u-beam",
        "memory": {
            "memory_id": memory_id,
            "idempotency_key": memory_id,
            "tenant_id": "t-beam",
            "user_id": "u-beam",
            "visibility": "tenant",
            "permission": {"owner": "u-beam"},
            "permission_level": "",
            "memory_kind": "fact",
            "fact": fact_text,
            "reference_frame": {
                "tenant": "t-beam",
                "user": "u-beam",
                "scene": "beam-holdout",
            },
            "recorded_at": "0001-01-01T00:00:00Z",
            "confidence": 0.9,
            "source": "chat",
            "version": 1,
            "provenance": "normal",
            "source_evidence": {
                "origin": "beam-holdout",
                "snippet": question,
            },
            "vector_status": "",
            "is_historical": False,
            "layer": "L1",
        },
    }


def build_beam_holdout(used_ids):
    """从 beam raw 中提取未使用条目并适配。"""
    raw_path = os.path.join(RAW_DIR, "beam.json")
    raw_data = load_json(raw_path)
    print(f"[beam] raw total: {len(raw_data)}")

    # 过滤已使用的
    available = []
    used_count = 0
    for item in raw_data:
        mid = f"beam-{item['question_id']}"
        if mid in used_ids:
            used_count += 1
            continue
        available.append(item)

    print(f"[beam] used in MRR: {used_count}, available: {len(available)}")
    return available


def main():
    print("=" * 60)
    print("Building real-corpus holdout for Polaris M1 formal evaluation")
    print("=" * 60)

    used_ids = collect_used_ids()

    # ---- Beam ----
    beam_available = build_beam_holdout(used_ids)

    # ---- 随机抽样（确定性 seed=42） ----
    rng = random.Random(SEED)
    rng.shuffle(beam_available)

    # 取前 N 条
    take = min(TARGET_COUNT, len(beam_available))
    selected = beam_available[:take]
    print(f"\n[sample] selected {len(selected)} beam items (seed={SEED})")

    # ---- 适配 ----
    holdout_items = []
    for item in selected:
        holdout_items.append(adapt_beam_item(item))

    # ---- 输出统计 ----
    bucket_counts = {}
    for it in holdout_items:
        b = it["bucket"]
        bucket_counts[b] = bucket_counts.get(b, 0) + 1

    print(f"\n[stats] total holdout items: {len(holdout_items)}")
    print("[stats] bucket distribution:")
    for b, c in sorted(bucket_counts.items()):
        print(f"  {b}: {c}")

    # ---- 写入文件 ----
    with open(OUTPUT, "w", encoding="utf-8") as f:
        for item in holdout_items:
            f.write(json.dumps(item, ensure_ascii=False) + "\n")

    print(f"\n[output] written to: {OUTPUT}")
    print("=" * 60)


if __name__ == "__main__":
    main()
