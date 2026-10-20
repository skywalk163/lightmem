// 构建版本信息（R29-E：启动自检的「二进制 vs 库」一致性比对）。
//
// 注入方式（部署链 install_lims_rc_d.sh 构建内核时追加）：
//
//	go build -ldflags "-X github.com/lightmem/kernel.BuildVersion=$(git rev-parse HEAD)" \
//	    -o lightmem-kernel ./cmd/lightmem-kernel
//
// 未注入时默认 unknown —— 启动自检的 VERSION 一致性检查退化为「不判定」
// （返回 true，绝不误报）。这与「未设 env = 默认现状」的红线④口径一致。
package kernel

// BuildVersion 是本二进制的源码 commit（ldflags 注入；默认 unknown）。
var BuildVersion = "unknown"

// SetBuildVersion 供测试或运行期覆写构建版本（仅进程内生效）。
func SetBuildVersion(v string) { BuildVersion = v }
