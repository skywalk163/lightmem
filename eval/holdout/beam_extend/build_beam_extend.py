#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
build_beam_extend.py — R12-B：从 BEAM raw 切出剩余未使用子集，构造 BEAM 扩展 holdout。

背景（R12 任务书 §三·B 路）：
  - BEAM raw 共 700 条；其中 100 条已用于公开 MRR（eval/benchmarks/dataset/beam.jsonl），
    300 条已进入 R11 真实语料 holdout（eval/holdout/holdout.jsonl）。
  - 剩余 300 条未被任何评测占用，本脚本将其提取并适配成与现有 BEAM holdout
    **完全一致**的 HoldoutItem 格式，作为高覆盖率语料扩充。

输出：
  - eval/holdout/beam_extend/holdout_beam_extend.jsonl（300 条）
  - eval/holdout/beam_extend/corpus_beam_extend.txt（query+fact 文本，供 gen_vectors.py 生成静态向量）

设计原则：
  1. 确定性：seed=42 固定随机种子，同输入逐位一致；
  2. 不污染：严格排除已用于 MRR 的 100 条 ID 与已入 R11 holdout 的 300 条 ID；
  3. 只增不改：只读现有评测文件，不修改 holdout.jsonl / dataset / raw。
"""

import json
import os
import random

# ---- 路径配置 ----
BASE = os.path.dirname(os.path.abspath(__file__))
HOLDOUT_DIR = os.path.dirname(BASE)                      # eval/holdout
EVAL_DIR = os.path.dirname(HOLDOUT_DIR)                  # eval
BENCH_DIR = os.path.join(EVAL_DIR, "benchmarks")
RAW_PATH = os.path.join(BENCH_DIR, "raw", "beam.json")
DATASET_BEAM = os.path.join(BENCH_DIR, "dataset", "beam.jsonl")
R11_HOLDOUT = os.path.join(HOLDOUT_DIR, "holdout.jsonl")

OUT_JSONL = os.path.join(BASE, "holdout_beam_extend.jsonl")
OUT_CORPUS = os.path.join(BASE, "corpus_beam_extend.txt")

SEED = 42

# question_type → bucket 映射（与 R11 build_holdout.py 保持一致）
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


def load_json(path):
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def load_jsonl(path):
    items = []
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                items.append(json.loads(line))
    return items


def collect_excluded_ids():
    """收集所有已被占用的 BEAM memory_id：MRR 100 条 + R11 holdout 300 条。"""
    excluded = set()
    mrr = 0
    for item in load_jsonl(DATASET_BEAM):
        excluded.add(item["id"])
        mrr += 1
    ho = 0
    for item in load_jsonl(R11_HOLDOUT):
        if item["id"].startswith("beam-"):
            excluded.add(item["id"])
            ho += 1
    print(f"[excluded] MRR beam ids: {mrr}, R11 holdout beam ids: {ho}, total: {len(excluded)}")
    return excluded


def adapt_beam_item(raw_item):
    """将 raw beam 条目适配成 HoldoutItem（与 R11 holdout 格式逐字段一致）。"""
    qid = raw_item["question_id"]
    qtype = raw_item["question_type"]
    question = raw_item["question"]
    rubric = raw_item.get("rubric", [])
    ground_truth = raw_item.get("ground_truth_answer", "")

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


def main():
    print("=" * 60)
    print("R12-B: build BEAM extend holdout (remaining unused raw subset)")
    print("=" * 60)

    excluded = collect_excluded_ids()

    raw_data = load_json(RAW_PATH)
    print(f"[beam] raw total: {len(raw_data)}")

    available = []
    for item in raw_data:
        mid = f"beam-{item['question_id']}"
        if mid in excluded:
            continue
        available.append(item)
    print(f"[beam] available (unused): {len(available)}")

    # 确定性排序（seed=42 shuffle），保证同输入逐位一致
    rng = random.Random(SEED)
    rng.shuffle(available)

    holdout_items = [adapt_beam_item(it) for it in available]

    # 输出统计
    bucket_counts = {}
    for it in holdout_items:
        bucket_counts[it["bucket"]] = bucket_counts.get(it["bucket"], 0) + 1
    print(f"\n[stats] total: {len(holdout_items)}")
    print("[stats] bucket distribution:")
    for b in sorted(bucket_counts):
        print(f"  {b}: {bucket_counts[b]}")

    # 唯一性 / 非污染自检
    ids = [it["id"] for it in holdout_items]
    assert len(ids) == len(set(ids)), "duplicate ids in extend subset"
    assert not (set(ids) & excluded), "extend subset overlaps used ids"

    os.makedirs(BASE, exist_ok=True)
    with open(OUT_JSONL, "w", encoding="utf-8") as f:
        for it in holdout_items:
            f.write(json.dumps(it, ensure_ascii=False) + "\n")
    print(f"\n[output] jsonl written: {OUT_JSONL}")

    # 向量语料：每条 query + memory.fact（去重保序），供 gen_vectors.py 生成静态向量。
    # corpus.txt 是行式文件，若文本内嵌换行会被拆成两行、导致该文本查不到向量；
    # 此处把内嵌换行替换成空格——与 Go 侧 normalizeVectorKey（空白折叠）语义一致，
    # 保证 Embed 查表键与写入键逐字符相同。
    seen = set()
    texts = []
    for it in holdout_items:
        for t in (it["query"], it["memory"]["fact"]):
            line = t.replace("\r", " ").replace("\n", " ")
            key = " ".join(line.lower().split())
            if not key or key in seen:
                continue
            seen.add(key)
            texts.append(line)
    with open(OUT_CORPUS, "w", encoding="utf-8") as f:
        for t in texts:
            f.write(t + "\n")
    print(f"[output] corpus written: {OUT_CORPUS} ({len(texts)} unique texts)")
    print("=" * 60)


if __name__ == "__main__":
    main()
