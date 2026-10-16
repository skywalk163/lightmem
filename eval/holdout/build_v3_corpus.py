#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
build_v3_corpus.py — R16-S6：从 holdout_v3.jsonl 生成 BGE-m3 静态向量语料。

背景（R16 计划 §三 S6）：
  - holdout_v3.jsonl 是 840 条合成中文 L2 语料（24 实体 × 35 记忆），用于扩大
    北极星覆盖面；但主向量库（count=4595，R5/R12 合流）不含 v3 文本，
    `-embedder vector` 会因查表 miss 返回全零向量（见 kernel/embedder_vector.go:141）。
  - 本脚本按 build_beam_extend.py 同口径抽取「每条 query + memory.fact」文本，
    去重保序落盘，供 eval/vectors/gen_vectors_inc.py 增量生成、再 merge 进主库。

输出：
  - eval/holdout/v3_corpus.txt（唯一文本行式，供 gen_vectors_inc.py -in）

设计原则（与既有 build_*.py 一致）：
  1. 确定性：只读 holdout_v3.jsonl，无随机源，同输入逐位一致；
  2. 只增不改：不修改 holdout_v3.jsonl；
  3. 键归一化：内嵌换行替换为空格，键按小写折叠空白——与 Go 侧
     normalizeVectorKey / 远端 gen_vectors_inc.py 的 index 键逐字符一致。
"""

import json
import os

BASE = os.path.dirname(os.path.abspath(__file__))
IN_JSONL = os.path.join(BASE, "holdout_v3.jsonl")
OUT_CORPUS = os.path.join(BASE, "v3_corpus.txt")


def norm_key(s):
    return " ".join(s.lower().split())


def main():
    items = []
    with open(IN_JSONL, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                items.append(json.loads(line))

    seen = set()
    texts = []
    for it in items:
        cand = [it.get("query", "")]
        mem = it.get("memory") or {}
        cand.append(mem.get("fact", ""))
        for t in cand:
            if not t:
                continue
            line = t.replace("\r", " ").replace("\n", " ")
            key = norm_key(line)
            if not key or key in seen:
                continue
            seen.add(key)
            texts.append(line)

    with open(OUT_CORPUS, "w", encoding="utf-8") as f:
        for t in texts:
            f.write(t + "\n")

    print("build_v3_corpus: items=%d unique_texts=%d -> %s"
          % (len(items), len(texts), OUT_CORPUS))


if __name__ == "__main__":
    main()