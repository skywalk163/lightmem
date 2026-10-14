#!/usr/bin/env python3
"""
build_corpus.py — 从 holdout.jsonl + holdout_proxy.jsonl 提取所有需要嵌入的文本，
生成 corpus.txt 供 gen_vectors.py 使用。

提取范围：
  - 每条的 query
  - 每条的 memory.fact
  - 每条的 hop1_memory.fact / hop2_memory.fact / second_version.fact（如果有）
  - 去重，保持顺序
"""

import json
import os
import sys

BASE = os.path.dirname(os.path.abspath(__file__))
EVAL_DIR = os.path.join(BASE, "..")
DATASET_DIR = os.path.join(EVAL_DIR, "dataset")
OUTPUT = os.path.join(EVAL_DIR, "vectors", "corpus.txt")


def load_jsonl(path):
    items = []
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                items.append(json.loads(line))
    return items


def extract_texts(item):
    """从一条 HoldoutItem 中提取所有需要嵌入的文本。"""
    texts = []
    # query
    if item.get("query"):
        texts.append(item["query"])
    # memory.fact
    if item.get("memory") and item["memory"].get("fact"):
        texts.append(item["memory"]["fact"])
    # hop1 / hop2 / second_version
    for key in ["hop1_memory", "hop2_memory", "second_version"]:
        if item.get(key) and item[key].get("fact"):
            texts.append(item[key]["fact"])
    return texts


def main():
    all_texts = []
    seen = set()

    files = [
        os.path.join(BASE, "holdout.jsonl"),  # 真实语料
        os.path.join(DATASET_DIR, "holdout_proxy.jsonl"),  # 合成代理
        # 也把 benchmarks 的加进来，确保 MRR 测试也能用
        os.path.join(EVAL_DIR, "benchmarks", "dataset", "locomo.jsonl"),
        os.path.join(EVAL_DIR, "benchmarks", "dataset", "longmemeval.jsonl"),
        os.path.join(EVAL_DIR, "benchmarks", "dataset", "beam.jsonl"),
    ]

    for fpath in files:
        if not os.path.exists(fpath):
            print(f"skip (not found): {fpath}")
            continue
        items = load_jsonl(fpath)
        count = 0
        for item in items:
            for text in extract_texts(item):
                key = " ".join(text.lower().split())
                if key in seen:
                    continue
                seen.add(key)
                all_texts.append(text)
                count += 1
        print(f"  {os.path.basename(fpath)}: {len(items)} items, +{count} unique texts")

    # 写出
    os.makedirs(os.path.dirname(OUTPUT), exist_ok=True)
    with open(OUTPUT, "w", encoding="utf-8") as f:
        for t in all_texts:
            f.write(t + "\n")

    print(f"\nTotal unique texts: {len(all_texts)}")
    print(f"Written to: {OUTPUT}")
    print(f"File size: {os.path.getsize(OUTPUT) / 1024:.1f} KB")


if __name__ == "__main__":
    main()
