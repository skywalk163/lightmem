#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
R5-A：BGE-m3 静态向量生成脚本（在 0.88 FreeBSD 上执行）。

背景：Go 进程内无 onnxruntime / llama.cpp（FreeBSD 无法安装 onnxruntime wheel）。
本脚本用 torch + transformers 离线加载 BAAI/bge-m3，对评测集 query/fact 文本
生成确定性嵌入，落盘 .npy；Go 侧 kernel/embedder_vector.go 读取该文件，
实现「确定性嵌入 + 余弦相似度」的 semantic 信号。

红线（任务书 §0）：
  - 不调 LLM（BGE 嵌入属任务书 §0 例外条款允许的「确定性嵌入」，不做排序/事实判断）；
  - 模型路径固定、推理参数固定，同一文本必得同一向量（seed 无关、无 time.Now()）；
  - 结果逐位可复现：同一输入 .txt 在同一模型文件下重跑，.npy 逐位一致。

用法：
  python3 gen_vectors.py -in corpus.txt -out vectors.npy \
      -meta vectors_meta.json -index vectors_index.jsonl \
      -model BAAI/bge-m3

输入：corpus.txt（每行一条文本；空行忽略）。
输出：
  - vectors.npy        [N, 1024] float32，L2 归一化，行序与 corpus.txt 一致；
  - vectors_meta.json  VectorMeta（model/dim/count/pooling/normalized 等）；
  - vectors_index.jsonl  每行 {"text":..., "index":n}，供 Go 侧查表。
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
    ap.add_argument("-in", "--input", required=True, help="corpus.txt 路径")
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
    print("model=%s dim=%d params=%.2fM" % (args.model, dim, n_params / 1e6))

    all_vec = np.zeros((len(texts), dim), dtype=np.float32)
    for i in range(0, len(texts), args.batch):
        batch = texts[i:i + args.batch]
        enc = tok(batch, padding=True, truncation=True,
                  max_length=args.max_len, return_tensors="pt")
        with torch.no_grad():
            out = model(**enc)
            # 平均池化（bge-m3 官方推荐；query/passage 共用，保证同空间余弦可比）
            emb = out.last_hidden_state * enc["attention_mask"].unsqueeze(-1)
            emb = emb.sum(dim=1) / enc["attention_mask"].sum(dim=1, keepdim=True)
        v = emb.cpu().numpy()
        norm = np.linalg.norm(v, axis=1, keepdims=True)
        norm[norm == 0] = 1.0
        all_vec[i:i + args.batch] = v / norm
        if (i // args.batch) % 20 == 0:
            print("  %d/%d (%.0fs)" % (i, len(texts), time.time() - t0), flush=True)

    # 归一化校验
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
    }
    with open(args.meta, "w", encoding="utf-8") as f:
        json.dump(meta, f, ensure_ascii=False, indent=2)

    # index jsonl：text -> 行号（去重保留首现）
    seen = set()
    with open(args.index, "w", encoding="utf-8") as f:
        for i, t in enumerate(texts):
            key = " ".join(t.lower().split())
            if key in seen:
                continue
            seen.add(key)
            f.write(json.dumps({"text": t, "index": i}, ensure_ascii=False) + "\n")

    print("OK out=%s n=%d dim=%d elapsed=%.1fs" % (args.out, len(texts), dim, time.time() - t0))
    print("sample vec[0][:5] = %s" % all_vec[0][:5].tolist())


if __name__ == "__main__":
    main()