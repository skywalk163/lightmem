#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
merge_vectors_inc.py — 把增量 vectors_inc.npy 追加到主 vectors.npy。

输入：
  - VEC_DIR/vectors.npy       旧向量 [2756, 1024]（来自 R5-D，复用）
  - VEC_DIR/vectors_meta.json 旧元数据
  - VEC_DIR/vectors_index.jsonl 旧索引（可选，追加文本索引时用到）
  - VEC_DIR/corpus_inc.txt    新增 1241 条
  - VEC_DIR/vectors_inc.npy   增量向量 [1241, 1024]
  - VEC_DIR/vectors_inc_meta.json
输出（覆盖主文件）：
  - vectors.npy       [3997, 1024]
  - vectors_meta.json count=3997
  - vectors_index.jsonl 追加增量索引
"""
import argparse
import json
import os
import sys
import time

import numpy as np


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-dir", "--dir", required=True, help="VEC_DIR（主 npy/meta/index 所在目录）")
    ap.add_argument("--backup", action="store_true", help="合并前备份旧 vectors.npy")
    args = ap.parse_args()

    d = args.dir
    old_npy = os.path.join(d, "vectors.npy")
    inc_npy = os.path.join(d, "vectors_inc.npy")

    if not os.path.exists(old_npy):
        print("FATAL: missing %s" % old_npy); sys.exit(1)
    if not os.path.exists(inc_npy):
        print("FATAL: missing %s" % inc_npy); sys.exit(1)

    old = np.load(old_npy)
    inc = np.load(inc_npy)
    print("old=%s %s" % (old.shape, old.dtype))
    print("inc=%s %s" % (inc.shape, inc.dtype))

    if old.shape[1] != inc.shape[1]:
        print("FATAL: dim mismatch old=%d inc=%d" % (old.shape[1], inc.shape[1])); sys.exit(1)
    if inc.shape[1] != 1024:
        print("FATAL: unexpected dim %d" % inc.shape[1]); sys.exit(1)

    if args.backup:
        bak = old_npy + ".bak_r12a"
        import shutil
        shutil.copyfile(old_npy, bak)
        print("backup ->", bak)

    merged = np.concatenate([old, inc], axis=0)
    assert merged.shape == (old.shape[0] + inc.shape[0], 1024)
    np.save(old_npy, merged)
    print("saved vectors.npy shape=%s dtype=%s" % (merged.shape, merged.dtype))

    meta_path = os.path.join(d, "vectors_meta.json")
    if os.path.exists(meta_path):
        with open(meta_path, encoding="utf-8") as f:
            meta = json.load(f)
        meta["count"] = int(merged.shape[0])
        meta["merged_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        meta["inc_count"] = int(inc.shape[0])
        with open(meta_path, "w", encoding="utf-8") as f:
            json.dump(meta, f, ensure_ascii=False, indent=2)
        print("meta count ->", meta["count"])
    else:
        print("WARN: no vectors_meta.json, skip")

    # 追加增量索引（关键：增量行号是相对 0..N-1，合并后必须 +offset 变绝对行号，
    # 否则 Embed 查表会指向旧向量矩阵的行，产生语义错乱）
    inc_idx = os.path.join(d, "vectors_inc_index.jsonl")
    idx_path = os.path.join(d, "vectors_index.jsonl")
    offset = old.shape[0]
    if os.path.exists(inc_idx):
        records = []
        with open(inc_idx, encoding="utf-8") as fi:
            for line in fi:
                line = line.strip()
                if not line:
                    continue
                try:
                    rec = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if "index" in rec and isinstance(rec["index"], int):
                    rec["index"] = rec["index"] + offset
                records.append(json.dumps(rec, ensure_ascii=False))
        if os.path.exists(idx_path):
            with open(idx_path, "a", encoding="utf-8") as fo:
                for r in records:
                    fo.write(r + "\n")
            print("index appended +%d (offset=%d)" % (len(records), offset))
        else:
            with open(idx_path, "w", encoding="utf-8") as fo:
                for r in records:
                    fo.write(r + "\n")
            print("index created %d (offset=%d)" % (len(records), offset))
    else:
        print("WARN: no vectors_inc_index.jsonl, index not updated")

    print("OK total=%d dim=%d" % (merged.shape[0], merged.shape[1]))


if __name__ == "__main__":
    main()