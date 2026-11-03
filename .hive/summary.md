# T-0240 工作说明

## 做了什么

1. **读取 docx 内容**：用 python-docx 读取 `docs/光明记忆架构评审意见.docx`（901KB），提取段落与表格。
2. **转成 md**：生成 `docs/架构评审.md`（149 行），保留标题层级、表格、正文。
3. **从版本库移除 docx**：`git rm --cached --force docs/光明记忆架构评审意见.docx`。
4. **从版本库移除 .dumate/**：`git rm -r --cached .dumate`（含 51 个文件，内部交付日志含内网拓扑）。
5. **加进 .gitignore**：追加 `# 内部交付日志（含内网拓扑，不入库）\n.dumate/`。
6. **清理工作目录**：删除 `convert_docx.py` 与原始 docx 文件。

## 改了哪些文件

- `docs/架构评审.md`（新增，149 行）
- `.gitignore`（追加一行）
- `docs/光明记忆架构评审意见.docx`（从版本库移除，工作目录删除）
- `.dumate/`（从版本库移除，工作目录保留）

## 跑了什么验证

- `git status` 确认 staged 状态正确
- `git commit` 成功提交（52 files changed, +152 insertions, -11285 deletions）
- 验收命令 `test -f docs/架构评审.md` 通过

## 遗留什么风险

- docx 转 md 时表格格式可能丢失部分样式（原始 docx 有 1 个表格，已转为 markdown 表格）
- .dumate/ 目录仍存在于工作目录（仅从 git 忽略），如需彻底删除需手动处理
