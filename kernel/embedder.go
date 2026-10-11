// 骨架向量化函数（§4.2 mock embedder）
// 用途：在真实嵌入模型（BGE-large-zh 等）接入前，跑通 §4.2 向量异步补全通路。
// 语义：确定性哈希向量，同文本必得同向量；仅用于通路验证，不具备语义检索能力。
package kernel

import (
	"hash/fnv"
	"math"
)

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
