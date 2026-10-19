#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
gen_vectors_inc.py — R12-A 增量向量生成。

背景：A 路 corpus 前 2756 行与 0.88 上 R5-D 的 corpus 逐行一致，
已用 BGE-m3 生成过向量（vectors.npy count=2756）。BGE 是确定性嵌入，
同一文本必得同一向量。因此只对新增 1241 行做嵌入，追加到旧 npy 之后，
避免重跑全量 3997 条（省 ~70% 时间）。

输入：corpus_inc.txt（新增文本，每行一条；空行忽略）
输出：
  - vectors_inc.npy        [N, 1024] float32，L2 归一化，行序与 corpus_inc.txt 一致
  - vectors_inc_meta.json  增量元数据（与主 meta 同 schema）
  - vectors_inc_index.jsonl 新增文本索引
"""
import argparse
import json
import os
import sys
import time

os.environ.setdefault("HF_ENDPOINT", "https://hf-mirror.com")
os.environ.setdefault("TOKENIZERS_PARALLELISM", "false")
os.environ.setdefault("OMP_NUM_THREADS", "4")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-in", "--input", required=True, help="增量 corpus.txt")
    ap.add_argument("-out", "--out", required=True, help=".npy 输出路径")
    ap.add_argument("-meta", "--meta", required=True, help="meta json 输出路径")
    ap.add_argument("-index", "--index", required=True, help="index jsonl 输出路径")
    ap.add_argument("-model", "--model", default="BAAI/bge-m3")
    ap.add_argument("--max-len", type=int, default=512)
    ap.add_argument("--batch", type=int, default=16)
    args = ap.parse_args()

    with open(args.input, encoding="utf-8") as f:
        texts = [l.rstrip("\n") for l in f if l.strip() != ""]
    if not texts:
        print("FATAL: empty corpus")
        sys.exit(1)

    import numpy as np
    import torch
    from transformers import AutoTokenizer, AutoModel

    t0 = time.time()
    tok = AutoTokenizer.from_pretrained(args.model)
    model = AutoModel.from_pretrained(args.model)
    model.eval()
    dim = model.config.hidden_size
    n_params = sum(p.numel() for p in model.parameters())
    print("model=%s dim=%d params=%.2fM n=%d" % (args.model, dim, n_params / 1e6, len(texts)), flush=True)

    all_vec = np.zeros((len(texts), dim), dtype=np.float32)
    for i in range(0, len(texts), args.batch):
        batch = texts[i:i + args.batch]
        enc = tok(batch, padding=True, truncation=True,
                  max_length=args.max_len, return_tensors="pt")
        with torch.no_grad():
            out = model(**enc)
            emb = out.last_hidden_state * enc["attention_mask"].unsqueeze(-1)
            emb = emb.sum(dim=1) / enc["attention_mask"].sum(dim=1, keepdim=True)
        v = emb.cpu().numpy()
        norm = np.linalg.norm(v, axis=1, keepdims=True)
        norm[norm == 0] = 1.0
        all_vec[i:i + args.batch] = v / norm
        print("  %d/%d (%.0fs)" % (i, len(texts), time.time() - t0), flush=True)

    norms = np.linalg.norm(all_vec, axis=1)
    assert np.allclose(norms, 1.0, atol=1e-3), "vector not normalized"

    os.makedirs(os.path.dirname(os.path.abspath(args.out)) or ".", exist_ok=True)
    np.save(args.out, all_vec)

    meta = {
        "model": args.model,
        "dim": int(dim),
        "count": len(texts),
        "pooling": "mean",
        "normalized": True,
        "query_prefix": "",
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "elapsed_s": round(time.time() - t0, 1),
        "n_params": n_params,
        "is_increment": True,
    }
    with open(args.meta, "w", encoding="utf-8") as f:
        json.dump(meta, f, ensure_ascii=False, indent=2)

    seen = set()
    with open(args.index, "w", encoding="utf-8") as f:
        for i, t in enumerate(texts):
            key = " ".join(t.lower().split())
            if key in seen:
                continue
            seen.add(key)
            f.write(json.dumps({"text": t, "index": i}, ensure_ascii=False) + "\n")

    print("OK out=%s n=%d dim=%d elapsed=%.1fs" % (args.out, len(texts), dim, time.time() - t0), flush=True)


if __name__ == "__main__":
    main()