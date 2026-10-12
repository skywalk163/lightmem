// 骨架向量化函数（§4.2 mock embedder）
// 用途：在真实嵌入模型（BGE-large-zh 等）接入前，跑通 §4.2 向量异步补全通路。
// 语义：确定性哈希向量，同文本必得同向量；仅用于通路验证，不具备语义检索能力。
package kernel

import (
	"errors"
	"hash/fnv"
	"math"
)

// Embedder 是可插拔向量化器抽象（R4-D 新增扩展点）。
//
// 背景（R3-A §8 风险表）：semantic 权重 2.0 当前依赖 char n-gram TF-IDF
// （CharNgramVocab）；未来接入真实向量模型（BGE 等）时，RankFusion 应能
// 不改动融合逻辑即切换实现。本接口定义该扩展点，**不改变现行为**：
// 现有 RankFusion.SetEmbedder（函数签名）仍可用，内部适配为 Embedder 存储。
//
// 红线遵守：接口仅描述向量化能力，不引入 LLM、不引入随机源、不改 Memory/RouterContext 语义。
type Embedder interface {
	// Embed 把文本向量化；空文本应返回固定长度全零向量（确定性）。
	// 实现必须对同一输入逐位一致（seed 无关，不允许 time.Now() / map 迭代序）。
	Embed(text string) ([]float32, error)
	// Dim 返回向量维度；固定词表实现返回词表大小。未知维度返回 0。
	Dim() int
	// Name 返回实现名（如 "char-ngram-tfidf-trigram" / "bge-large-zh"），供日志与诊断。
	Name() string
}

// EmbedderFunc 把普通向量化函数适配为 Embedder（向后兼容 R3-A 的函数式注入）。
//
// RankFusion.SetEmbedder 历史签名接受 func(string) ([]float32, error)；
// 本适配器让旧调用方零改动即可接入新的接口存储，同时为未来接口式注入（SetEmbedderImpl）
// 提供统一类型。Dim 未知时返回 0（融合层不依赖 Dim，仅用于诊断日志）。
type EmbedderFunc func(text string) ([]float32, error)

// Embed 实现 Embedder 接口：直接委托给底层函数。
func (f EmbedderFunc) Embed(text string) ([]float32, error) {
	if f == nil {
		return nil, errors.New("embedder: nil func")
	}
	return f(text)
}

// Dim 实现 Embedder 接口：函数式注入的维度未知，返回 0。
func (f EmbedderFunc) Dim() int { return 0 }

// Name 实现 Embedder 接口。
func (f EmbedderFunc) Name() string { return "embedder-func" }

// HashEmbedder 返回一个确定性向量化函数。
// dim 为向量维度；返回的函数对同一输入始终产出相同向量（已归一化，可用于余弦检索）。
func HashEmbedder(dim int) func(string) ([]float32, error) {
	if dim <= 0 {
		dim = 256
	}
	return func(text string) ([]float32, error) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(text))
		seed := h.Sum64()

		vec := make([]float32, dim)
		var sum float64
		for i := 0; i < dim; i++ {
			// 用 LCG 从 seed 生成伪随机序列
			seed = seed*6364136223846793005 + 1442695040888963407
			val := float64((seed>>33)&0xFFFF)/65535.0*2 - 1
			vec[i] = float32(val)
			sum += val * val
		}
		// 归一化到单位长度（余弦相似度需要）
		norm := math.Sqrt(sum)
		if norm > 0 {
			for i := range vec {
				vec[i] = float32(float64(vec[i]) / norm)
			}
		}
		return vec, nil
	}
}
