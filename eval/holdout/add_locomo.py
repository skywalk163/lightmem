#!/usr/bin/env python3
"""
add_locomo.py — 从 locomo raw 中提取未使用的 QA，追加到 holdout.jsonl。

locomo 结构：
  - 10 个对话，每个对话有 ~200 条 QA
  - 每条 QA 有 question, answer, evidence (如 "D15:14"), category
  - evidence 指向对话消息：D{n}:{m} = session_{n} 的第 m 条消息（dia_id 从 1 开始）
"""

import json
import random
import os
import re

BASE = os.path.dirname(os.path.abspath(__file__))
BENCH_DIR = os.path.join(BASE, "..", "benchmarks")
RAW_DIR = os.path.join(BENCH_DIR, "raw")
DATASET_DIR = os.path.join(BENCH_DIR, "dataset")
OUTPUT = os.path.join(BASE, "holdout.jsonl")

SEED = 42
TARGET_COUNT = 200  # 再补 200 条 locomo


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


def collect_used_locomo_ids():
    """收集已使用的 locomo ID。"""
    path = os.path.join(DATASET_DIR, "locomo.jsonl")
    used = set()
    if os.path.exists(path):
        for item in load_jsonl(path):
            used.add(item["id"])
    print(f"[locomo] used IDs: {len(used)}")
    return used


def build_dialog_index(conv_data):
    """
    从对话数据中构建 dia_id → message 的索引。
    返回: dict[dia_id] = {speaker, text}
    """
    inner = conv_data["conversation"]
    index = {}
    # 遍历所有 session
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


# category → bucket 映射（从已使用数据反推）
# category 1: single fact (single-hop)
# category 2: temporal (时间相关)
# category 3: multi-hop / reasoning
CATEGORY_TO_BUCKET = {
    1: "single-hop",
    2: "temporal",
    3: "multi-hop",
}


def adapt_locomo_qa(conv_id, qa, dialog_index, qa_idx):
    """
    将 locomo 原始 QA 适配成 HoldoutItem。
    conv_id: 如 "conv-44"
    qa: 原始 QA dict
    dialog_index: dia_id → message 索引
    qa_idx: QA 在对话中的序号（用于生成唯一 ID）
    """
    question = qa.get("question", "")
    if not question:
        return None
    answer = str(qa.get("answer", qa.get("expected_answer", "")))
    if not answer or answer == "None":
        return None
    evidence = qa.get("evidence", [])
    category = qa.get("category", 1)

    # 取第一个 evidence 对应的消息文本作为 memory.fact
    fact_text = ""
    memory_ids = []
    occurred_at = ""

    for ev in evidence:
        # ev 格式: "D15:14"
        match = re.match(r"D(\d+):(\d+)", ev)
        if not match:
            continue
        dia_id = ev
        msg = dialog_index.get(dia_id)
        if msg:
            speaker = msg.get("speaker", "")
            text = msg.get("text", "")
            if fact_text:
                fact_text += " "
            fact_text += f"{speaker}: {text}"
            memory_ids.append(f"lc-{conv_id}-{dia_id.replace(':', '_')}")

    if not fact_text:
        # fallback：直接用 question + answer 构造
        fact_text = f"{question} => {answer}"

    bucket = CATEGORY_TO_BUCKET.get(category, "single-hop")

    # memory_id：用第一个 evidence 的 dia_id
    if memory_ids:
        primary_id = memory_ids[0]
    else:
        primary_id = f"lc-{conv_id}-qa{qa_idx}"

    return {
        "id": primary_id,
        "query": question,
        "expected_fact": answer,
        "expected_memory_ids": memory_ids if memory_ids else [primary_id],
        "bucket": bucket,
        "is_temporal": bucket == "temporal",
        "has_injection": False,
        "has_pollution": False,
        "tenant": "t-lc-holdout",
        "user": "u-lc-holdout",
        "memory": {
            "memory_id": primary_id,
            "idempotency_key": primary_id,
            "tenant_id": "t-lc-holdout",
            "user_id": "u-lc-holdout",
            "visibility": "tenant",
            "permission": {"owner": "u-lc-holdout"},
            "permission_level": "",
            "memory_kind": "fact",
            "fact": fact_text,
            "reference_frame": {
                "tenant": "t-lc-holdout",
                "user": "u-lc-holdout",
                "scene": "locomo-holdout",
            },
            "occurred_at": occurred_at or "0001-01-01T00:00:00Z",
            "recorded_at": "0001-01-01T00:00:00Z",
            "confidence": 0.9,
            "source": "chat",
            "version": 1,
            "provenance": "normal",
            "source_evidence": {
                "origin": "locomo-holdout",
                "snippet": question,
            },
            "vector_status": "",
            "is_historical": False,
            "layer": "L1",
        },
    }


def main():
    print("=" * 60)
    print("Adding locomo holdout items")
    print("=" * 60)

    used_ids = collect_used_locomo_ids()

    # 读取 raw locomo
    raw_path = os.path.join(RAW_DIR, "locomo.json")
    raw_data = load_json(raw_path)
    print(f"[locomo] raw conversations: {len(raw_data)}")

    # 遍历所有对话的所有 QA
    all_available = []
    total_qa = 0
    used_count = 0

    for conv in raw_data:
        conv_id = conv["sample_id"]  # 如 "conv-26"
        dialog_index = build_dialog_index(conv)
        qas = conv.get("qa", [])
        total_qa += len(qas)

        for idx, qa in enumerate(qas):
            # 构造预期 ID（和已使用的格式对齐）
            evidence = qa.get("evidence", [])
            if evidence:
                dia_id = evidence[0]
                expected_id = f"lc-{conv_id}-{dia_id.replace(':', '_')}"
            else:
                expected_id = f"lc-{conv_id}-qa{idx}"

            if expected_id in used_ids:
                used_count += 1
                continue

            all_available.append({
                "conv_id": conv_id,
                "qa": qa,
                "dialog_index": dialog_index,
                "qa_idx": idx,
            })

    print(f"[locomo] total QA: {total_qa}, used: {used_count}, available: {len(all_available)}")

    # 随机抽样
    rng = random.Random(SEED + 1)  # 和 beam 的 seed 区分开
    rng.shuffle(all_available)

    take = min(TARGET_COUNT, len(all_available))
    selected = all_available[:take]
    print(f"[sample] selected {len(selected)} locomo items")

    # 适配
    new_items = []
    skipped = 0
    for item in selected:
        adapted = adapt_locomo_qa(
            item["conv_id"],
            item["qa"],
            item["dialog_index"],
            item["qa_idx"],
        )
        if adapted is None:
            skipped += 1
            continue
        new_items.append(adapted)
    if skipped:
        print(f"[adapt] skipped {skipped} invalid QA items")

    # 追加到现有 holdout.jsonl
    existing = load_jsonl(OUTPUT)
    print(f"\n[merge] existing holdout: {len(existing)} items")

    # 合并，检查重叠
    existing_ids = {it["id"] for it in existing}
    added = 0
    for item in new_items:
        if item["id"] not in existing_ids:
            existing.append(item)
            existing_ids.add(item["id"])
            added += 1

    print(f"[merge] added: {added} new locomo items")
    print(f"[merge] total now: {len(existing)} items")

    # 统计 bucket 分布
    bucket_counts = {}
    for it in existing:
        b = it["bucket"]
        bucket_counts[b] = bucket_counts.get(b, 0) + 1
    print("\n[stats] final bucket distribution:")
    for b, c in sorted(bucket_counts.items()):
        print(f"  {b}: {c}")

    # 写回
    with open(OUTPUT, "w", encoding="utf-8") as f:
        for item in existing:
            f.write(json.dumps(item, ensure_ascii=False) + "\n")

    print(f"\n[output] updated: {OUTPUT}")
    print("=" * 60)


if __name__ == "__main__":
    main()
