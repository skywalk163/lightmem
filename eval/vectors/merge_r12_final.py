#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
merge_r12_final.py — R12 收口合流：把 A 路 corpus/npy 与 B 路 corpus/npy 合并，
生成合流后的 holdout_v2.jsonl 与合并向量，供最终北极星判定使用。

背景：
  - A 路 corpus.txt（3995 唯一文本）覆盖 现有 holdout 450 + locomo_v2 200
  - B 路 corpus_beam_extend.txt（600 唯一文本）覆盖 beam_extend 300
  - 合流 holdout = 现有 holdout 450 + beam_extend 300 + locomo_v2 200 = 800
    其中现有 holdout 里的 LoCoMo 150 由 locomo_v2 200 替换（任务书 §3 合流定义）
  - B 路向量已生成好（count=600），直接复用，只需拼 A npy + B npy

输出（全部写入 -out-dir）：
  - corpus_merge.txt          合并语料（去重保序）
  - vectors_merge.npy         合并向量矩阵 [A+B, 1024]
  - vectors_merge_meta.json   合并元数据（count = A+B）
  - vectors_merge_index.jsonl 合并索引（B 部分行号 + offset_A）
  - holdout_v2.jsonl          合流 holdout（450 + 300 + 200 = 950? 见 --mode）
"""
import argparse
import json
import os
import shutil
import sys

import numpy as np


def load_unique(path):
    """读取文本文件，返回 (唯一文本列表, 去重后的顺序)。"""
    out = []
    seen = set()
    with open(path, encoding="utf-8") as f:
        for line in f:
            t = line.strip()
            if not t:
                continue
            key = " ".join(t.lower().split())
            if key in seen:
                continue
            seen.add(key)
            out.append(t)
    return out


def load_jsonl(path):
    items = []
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                items.append(json.loads(line))
    return items


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-a-vec-dir", required=True, help="A 路 eval/vectors 目录")
    ap.add_argument("-a-holdout", required=True, help="A 路现有 holdout.jsonl 路径")
    ap.add_argument("-a-locomo", required=True, help="A 路 locomo_v2 holdout 路径")
    ap.add_argument("-b-vec-dir", required=True, help="B 路 eval/vectors 目录")
    ap.add_argument("-b-holdout", required=True, help="B 路 beam_extend holdout 路径")
    ap.add_argument("-out-dir", required=True, help="输出目录（通常 /tmp/r12/lightmem/eval/vectors）")
    ap.add_argument("-out-holdout", required=True, help="合并 holdout 输出路径")
    ap.add_argument("--mode", default="merge",
                    choices=["merge", "concat"],
                    help="merge=450+300+200；concat=450+300+200 不替换 LoCoMo")
    args = ap.parse_args()

    os.makedirs(args.out_dir, exist_ok=True)

    # ---------- 1. 合并 corpus ----------
    # A corpus 路径优先 corpus_a.txt（合流目录命名），回退 corpus.txt
    a_corpus_path = os.path.join(args.a_vec_dir, "corpus_a.txt")
    if not os.path.exists(a_corpus_path):
        a_corpus_path = os.path.join(args.a_vec_dir, "corpus.txt")
    # B corpus 路径优先 corpus_beam_extend.txt，回退 corpus_b.txt
    b_corpus_path = os.path.join(args.b_vec_dir, "corpus_beam_extend.txt")
    if not os.path.exists(b_corpus_path):
        b_corpus_path = os.path.join(args.b_vec_dir, "corpus_b.txt")
    if not os.path.exists(b_corpus_path):
        # B 路原始 corpus 可能已删，尝试从 index 重建
        b_idx = os.path.join(args.b_vec_dir, "vectors_index.jsonl")
        if os.path.exists(b_idx):
            texts = []
            seen = set()
            with open(b_idx, encoding="utf-8") as f:
                for line in f:
                    r = json.loads(line)
                    t = r.get("text", "")
                    if t and t not in seen:
                        seen.add(t)
                        texts.append(t)
            b_corpus_path = os.path.join(args.out_dir, "corpus_beam_extend_rebuilt.txt")
            with open(b_corpus_path, "w", encoding="utf-8") as f:
                f.write("\n".join(texts) + "\n")
            print(f"[INFO] B corpus 已从 index 重建: {len(texts)} 条 -> {b_corpus_path}")
        else:
            raise SystemExit("B corpus.txt 与 index 均缺失")

    a_texts = load_unique(a_corpus_path)
    b_texts = load_unique(b_corpus_path)

    # 合并去重（A 优先）
    merged_texts = []
    seen = set()
    for t in a_texts + b_texts:
        key = " ".join(t.lower().split())
        if key in seen:
            continue
        seen.add(key)
        merged_texts.append(t)
    print(f"A={len(a_texts)} B={len(b_texts)} 合并去重后={len(merged_texts)}")

    corpus_out = os.path.join(args.out_dir, "corpus_merge.txt")
    with open(corpus_out, "w", encoding="utf-8") as f:
        f.write("\n".join(merged_texts) + "\n")

    # ---------- 2. 合并向量 ----------
    # A npy：优先 vectors_a.npy，回退 vectors.npy
    a_npy_path = os.path.join(args.a_vec_dir, "vectors_a.npy")
    if not os.path.exists(a_npy_path):
        a_npy_path = os.path.join(args.a_vec_dir, "vectors.npy")
    # B npy：优先 vectors_b.npy，回退 vectors.npy
    b_npy_path = os.path.join(args.b_vec_dir, "vectors_b.npy")
    if not os.path.exists(b_npy_path):
        b_npy_path = os.path.join(args.b_vec_dir, "vectors.npy")
    a_npy = np.load(a_npy_path)
    b_npy = np.load(b_npy_path)
    print(f"A npy={a_npy.shape} B npy={b_npy.shape}")
    if a_npy.shape[1] != b_npy.shape[1]:
        raise SystemExit(f"dim mismatch: {a_npy.shape[1]} vs {b_npy.shape[1]}")

    merged_vec = np.concatenate([a_npy, b_npy], axis=0)
    assert merged_vec.shape[1] == 1024
    npy_out = os.path.join(args.out_dir, "vectors_merge.npy")
    np.save(npy_out, merged_vec)
    # Go 侧 polaris 硬编码读 vectors.npy，同步一份标准名
    shutil.copyfile(npy_out, os.path.join(args.out_dir, "vectors.npy"))
    print(f"合并向量 -> {npy_out} {merged_vec.shape}")

    # ---------- 3. 合并索引（B 部分行号 + len(a_texts)） ----------
    offset_a = len(a_texts)
    # A meta：优先 vectors_meta_a.json，回退 vectors_meta.json
    a_meta_path = os.path.join(args.a_vec_dir, "vectors_meta_a.json")
    if not os.path.exists(a_meta_path):
        a_meta_path = os.path.join(args.a_vec_dir, "vectors_meta.json")
    a_meta = json.load(open(a_meta_path, encoding="utf-8"))
    meta = dict(a_meta)
    meta["count"] = int(merged_vec.shape[0])
    meta["merged"] = True
    meta["a_count"] = len(a_texts)
    meta["b_count"] = len(b_texts)
    meta["generated_at"] = meta.get("generated_at", "n/a")
    meta_out = os.path.join(args.out_dir, "vectors_merge_meta.json")
    with open(meta_out, "w", encoding="utf-8") as f:
        json.dump(meta, f, ensure_ascii=False, indent=2)
    # Go 侧读 vectors_meta.json
    shutil.copyfile(meta_out, os.path.join(args.out_dir, "vectors_meta.json"))

    index_out = os.path.join(args.out_dir, "vectors_merge_index.jsonl")
    # A index：优先 vectors_index_a.jsonl，回退 vectors_index.jsonl
    a_idx_path = os.path.join(args.a_vec_dir, "vectors_index_a.jsonl")
    if not os.path.exists(a_idx_path):
        a_idx_path = os.path.join(args.a_vec_dir, "vectors_index.jsonl")
    # B index：优先 vectors_index_b.jsonl，回退 vectors_index.jsonl
    b_idx_path = os.path.join(args.b_vec_dir, "vectors_index_b.jsonl")
    if not os.path.exists(b_idx_path):
        b_idx_path = os.path.join(args.b_vec_dir, "vectors_index.jsonl")
    with open(index_out, "w", encoding="utf-8") as fo:
        # A 部分：从 A index 读（若存在）否则用 corpus 顺序
        if os.path.exists(a_idx_path):
            with open(a_idx_path, encoding="utf-8") as fi:
                for line in fi:
                    line = line.strip()
                    if line:
                        fo.write(line + "\n")
        else:
            for i, t in enumerate(a_texts):
                fo.write(json.dumps({"text": t, "index": i}, ensure_ascii=False) + "\n")
        # B 部分：从 B index 读，行号 + offset_a
        with open(b_idx_path, encoding="utf-8") as fi:
            for line in fi:
                line = line.strip()
                if not line:
                    continue
                r = json.loads(line)
                r["index"] = r["index"] + offset_a
                fo.write(json.dumps(r, ensure_ascii=False) + "\n")
    print(f"合并索引 -> {index_out}")
    # Go 侧读 vectors_index.jsonl
    shutil.copyfile(index_out, os.path.join(args.out_dir, "vectors_index.jsonl"))

    # ---------- 4. 合并 holdout ----------
    a_hold = load_jsonl(args.a_holdout)          # 450
    b_hold = load_jsonl(args.b_holdout)          # 300
    a_locomo = load_jsonl(args.a_locomo)         # 200

    # 剔除现有 holdout 中的 LoCoMo lc-* 条目（由 locomo_v2 替换）
    kept = [it for it in a_hold if not it.get("id", "").startswith("lc-")]
    print(f"现有 holdout 450 -> 剔除 lc-* 后 {len(kept)}（BEAM 300）")

    merged_hold = kept + b_hold + a_locomo
    # ID 唯一性
    ids = [it.get("id", "") for it in merged_hold]
    assert len(ids) == len(set(ids)), f"ID 重复: {len(ids)} vs {len(set(ids))}"
    print(f"合并 holdout: BEAM300 + beam_extend300 + locomo_v2 200 = {len(merged_hold)}")

    with open(args.out_holdout, "w", encoding="utf-8") as f:
        for it in merged_hold:
            f.write(json.dumps(it, ensure_ascii=False) + "\n")
    print(f"合并 holdout -> {args.out_holdout}")

    # ---------- 5. 校验覆盖 ----------
    corpus_set = set(" ".join(t.lower().split()) for t in merged_texts)
    missing = 0
    for it in merged_hold:
        q = it.get("query", "")
        mf = (it.get("memory") or {}).get("fact", "")
        if q and " ".join(q.lower().split()) not in corpus_set:
            missing += 1
        if mf and " ".join(mf.lower().split()) not in corpus_set:
            missing += 1
    print(f"[COVERAGE] 合并 holdout query/fact 在 corpus 中缺失: {missing}")
    if missing > 0:
        print("[WARN] 有缺失，向量口径可能 miss；请检查")

    print("OK 合流完成")


if __name__ == "__main__":
    main()