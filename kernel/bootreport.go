// 启动自检（R29-T0 S0 接缝，骨架实现）。
//
// 职责边界（任务书 §2.2 / §10）：
//   - T0 落 BootReport 结构与 RunBootReport 骨架（只查「未完成标记」+ 回填后端名）。
//   - E 线硬化：列检查、版本匹配、RestorePoint 校验、与未完成标记的读取联动。
//
// S0 最小可用：
//   - Backend 填 backend.Name()；
//   - Unfinished 填 dataDir 下残留的 .drain.unfinished 标记（D 线 MarkUnfinished 写入）；
//   - Table/Columns/Rows/VersionMatch/RestorePoint 交 E 线填实。
//
// 红线②③：只用 stdlib，不新增依赖；只读检查，不改任何 Schema/SQL。
package kernel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// BootReport 启动自检结果（§10 冻结）。
type BootReport struct {
	Backend      string
	Table        string
	Columns      string
	Rows         int64
	VersionMatch bool
	Unfinished   string // 非空 = 上次有未完成的排水任务（残留标记）
	RestorePoint string
}

// RunBootReport 执行启动自检（§10 冻结签名）。
//
// S0：回填后端名 + 检查 dataDir 下的未完成标记。dataDir 为空时跳过标记检查
// （默认关：main.go 仅当显式配置 -data-dir 才调用，逐位保持 c203db8 行为）。
func RunBootReport(ctx context.Context, backend StorageBackend, dataDir string) (*BootReport, error) {
	rep := &BootReport{}
	if backend != nil {
		rep.Backend = backend.Name()
	}
	if dataDir == "" {
		return rep, nil
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return rep, nil
		}
		return rep, err
	}
	var unfinished []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".drain.unfinished") {
			job := strings.TrimSuffix(name, ".drain.unfinished")
			unfinished = append(unfinished, job)
		}
	}
	if len(unfinished) > 0 {
		rep.Unfinished = strings.Join(unfinished, ",")
	}
	return rep, nil
}

// bootReportMarkerPath 仅用于单测构造断言路径（包内可见）。
func bootReportMarkerPath(dataDir, job string) string {
	return filepath.Join(dataDir, job+".drain.unfinished")
}
