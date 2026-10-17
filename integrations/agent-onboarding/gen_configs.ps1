#Requires -Version 5.1
<#
.SYNOPSIS
    lightmem Agent 接入配置生成器（R21 D 线）。

.DESCRIPTION
    读取 configs/ 下的模板，把 token / 主机 / 端口 / 二进制路径替换成真实值，
    输出到 -OutDir。**只产出待粘贴文件，绝不写入任何 Agent 的配置目录**，
    因此不会误伤既有配置。

    输出目录里会额外生成 MANIFEST.md（生成文件 → 目标配置路径 → 粘贴方式），
    照着搬即可。

.PARAMETER OutDir
    输出目录。默认 <脚本目录>\out。

.PARAMETER McpHost
    lightmem 主机。默认 192.168.0.88。

.PARAMETER McpPort
    MCP-over-HTTP 端口（A 线原生端点）。默认 18444。

.PARAMETER RestPort
    业务 REST 端口（stdio 桥回落用）。默认 18443。

.PARAMETER TokenFile
    token 表 JSON（键 = agent 键，值 = token）。见 tokens.example.json。

.PARAMETER DefaultToken
    未在 TokenFile 中出现的 agent 统一用这个 token。

.PARAMETER Agents
    只生成指定 agent 的配置。默认全部。

.PARAMETER McpBin
    原生 lightmem-mcp 二进制路径（stdio 模板用）。默认 /usr/local/lightmem/lightmem-mcp。

.PARAMETER ProfilePath
    lightmem profile 路径（stdio 模板用）。默认 /usr/local/lightmem/profiles/lims-family.profile.yaml。

.PARAMETER PythonExe
    Python 解释器（stdio 桥模板用）。默认 C:\Python314\python.exe。

.PARAMETER BridgePy
    MCP 桥脚本路径（stdio 桥模板用）。

.PARAMETER TenantId / UserId
    stdio 桥模板里的默认租户 / 用户。

.PARAMETER Force
    允许覆盖 OutDir 中已存在的文件。默认拒绝。

.PARAMETER ListAgents
    只打印可生成的 agent 列表后退出。

.EXAMPLE
    .\gen_configs.ps1 -TokenFile .\tokens.example.json

.EXAMPLE
    .\gen_configs.ps1 -Agents dsh,workbuddy -McpHost 10.0.0.9 -DefaultToken 't-dsh-xxx' -Force

.NOTES
    兼容 Windows PowerShell 5.1（不用 ?? / ?: / -Encoding utf8NoBOM）。
    输出一律 UTF-8 **无 BOM**：带 BOM 的 JSON 会让部分 Agent 的解析器直接报错。
#>
[CmdletBinding()]
param(
    [string]$OutDir,
    [string]$McpHost = '192.168.0.88',
    [int]$McpPort = 18444,
    [int]$RestPort = 18443,
    [string]$TokenFile,
    [string]$DefaultToken = '',
    [string[]]$Agents,
    [string]$McpBin = '/usr/local/lightmem/lightmem-mcp',
    [string]$ProfilePath = '/usr/local/lightmem/profiles/lims-family.profile.yaml',
    [string]$PythonExe = 'C:\Python314\python.exe',
    [string]$BridgePy = 'G:\dswork\duan-light-merge\lightmem\integrations\dsh-mcp-bridge\lightmem_mcp_bridge.py',
    [string]$TenantId = 'dshteam',
    [string]$UserId = 'skywalk',
    [switch]$Force,
    [switch]$ListAgents
)

$ErrorActionPreference = 'Stop'

# ---------------------------------------------------------------------------
# 0. 路径与常量
# ---------------------------------------------------------------------------
$ScriptDir   = $PSScriptRoot
$TemplateDir = Join-Path $ScriptDir 'configs'

if ([string]::IsNullOrWhiteSpace($OutDir)) {
    $OutDir = Join-Path $ScriptDir 'out'
}

# agent 键 → 模板 / 输出名 / 身份默认值 / 目标配置路径 / 粘贴方式
# 目标路径里的 %USERPROFILE%、%APPDATA% 只用于 MANIFEST 展示，脚本不往里写。
$Map = @(
    [pscustomobject]@{
        Agent    = 'dsh'
        Template = 'dsh.cordis.patch.yml'
        Output   = 'dsh.cordis.patch.yml'
        TokenKey = 'dsh'
        AgentId  = 'deepseek-harness'
        UserId   = 'dsh-agent'
        Transport = 'streamable-http'
        Target   = '%USERPROFILE%\.dsh\profiles\desktop\cordis.patch.yml'
        Mode     = '追加到文件末尾（不可覆盖）'
    },
    [pscustomobject]@{
        Agent    = 'trae'
        Template = 'trae.mcp.json'
        Output   = 'trae.mcp.json'
        TokenKey = 'trae'
        AgentId  = 'trae'
        UserId   = $UserId
        Transport = 'http (url)'
        Target   = '%APPDATA%\Trae CN\User\mcp.json'
        Mode     = '整文件新建；已存在则合并 mcpServers'
    },
    [pscustomobject]@{
        Agent    = 'trae-solo'
        Template = 'trae-solo.mcp.json'
        Output   = 'trae-solo.mcp.json'
        TokenKey = 'trae-solo'
        AgentId  = 'trae-solo'
        UserId   = $UserId
        Transport = 'http (url)'
        Target   = '%APPDATA%\TRAE SOLO CN\User\mcp.json'
        Mode     = '整文件新建；已存在则合并 mcpServers'
    },
    [pscustomobject]@{
        Agent    = 'trae-workspace'
        Template = 'trae-workspace.mcp.json'
        Output   = 'trae-workspace.mcp.json'
        TokenKey = 'trae'
        AgentId  = 'trae'
        UserId   = $UserId
        Transport = 'http (url)'
        Target   = '<工作区根>\.trae\mcp.json'
        Mode     = '整文件新建（工作区级，优先于用户级）'
    },
    [pscustomobject]@{
        Agent    = 'workbuddy'
        Template = 'workbuddy.mcp.json'
        Output   = 'workbuddy.mcp.json'
        TokenKey = 'workbuddy'
        AgentId  = 'workbuddy'
        UserId   = $UserId
        Transport = 'streamableHttp'
        Target   = '%USERPROFILE%\.workbuddy\mcp.json'
        Mode     = '合并 lightmem 块到既有 mcpServers（保留其它条目）'
    },
    [pscustomobject]@{
        Agent    = 'raccoon'
        Template = 'raccoon.mcp.json'
        Output   = 'raccoon.mcp.json'
        TokenKey = 'raccoon'
        AgentId  = 'raccoon'
        UserId   = $UserId
        Transport = 'streamable_http'
        Target   = '%USERPROFILE%\.box-agent\config\mcp.json（用户层）'
        Mode     = '合并到用户层 mcpServers；勿改 mcp.system.json'
    },
    [pscustomobject]@{
        Agent    = 'opencode'
        Template = 'opencode.json'
        Output   = 'opencode.json'
        TokenKey = 'opencode'
        AgentId  = 'opencode'
        UserId   = $UserId
        Transport = 'remote'
        Target   = '<工作区根>\opencode.json（或 DuMate 的 resources\config\opencode\opencode.json）'
        Mode     = '合并顶层 mcp 块（保留 model/provider）'
    },
    [pscustomobject]@{
        Agent    = 'codearts'
        Template = 'codearts.mcp_settings.json'
        Output   = 'codearts.mcp_settings.json'
        TokenKey = 'codearts'
        AgentId  = 'codearts'
        UserId   = $UserId
        Transport = 'stdio (native)'
        Target   = '%USERPROFILE%\.codearts\mcp\mcp_settings.json'
        Mode     = '合并 mcp.lightmem 块；需先有 lightmem-mcp 可执行文件'
    },
    [pscustomobject]@{
        Agent    = 'codearts-bridge'
        Template = 'codearts.bridge.mcp_settings.json'
        Output   = 'codearts.bridge.mcp_settings.json'
        TokenKey = 'codearts'
        AgentId  = 'codearts'
        UserId   = $UserId
        Transport = 'stdio (bridge)'
        Target   = '%USERPROFILE%\.codearts\mcp\mcp_settings.json'
        Mode     = '过渡通道：Windows 上唯一可用（无需 Go 二进制）'
    },
    [pscustomobject]@{
        Agent    = 'roo-cline'
        Template = 'roo-cline.mcp_settings.json'
        Output   = 'roo-cline.mcp_settings.json'
        TokenKey = 'roo-cline'
        AgentId  = 'roo-cline'
        UserId   = $UserId
        Transport = 'stdio (native)'
        Target   = '%APPDATA%\Code\User\globalStorage\rooveterinaryinc.roo-cline\settings\mcp_settings.json'
        Mode     = '合并 mcpServers；便携版 VSCode 路径见 README'
    }
)

if ($ListAgents) {
    $Map | Select-Object Agent, Transport, Template, Target | Format-Table -AutoSize | Out-String -Width 200 | Write-Host
    return
}

# ---------------------------------------------------------------------------
# 1. 前置检查
# ---------------------------------------------------------------------------
if (-not (Test-Path -LiteralPath $TemplateDir)) {
    throw "模板目录不存在：$TemplateDir"
}

# token 表：agent 键 → token
$TokenMap = @{}
if (-not [string]::IsNullOrWhiteSpace($TokenFile)) {
    if (-not (Test-Path -LiteralPath $TokenFile)) {
        throw "TokenFile 不存在：$TokenFile"
    }
    $raw = Get-Content -LiteralPath $TokenFile -Raw -Encoding UTF8
    $obj = $raw | ConvertFrom-Json
    # 支持两种形状：扁平 {agent:token} 或 {"tokens":{agent:token}}
    $src = $obj
    if ($obj.PSObject.Properties.Name -contains 'tokens') { $src = $obj.tokens }
    foreach ($p in $src.PSObject.Properties) {
        if ($p.Name.StartsWith('_')) { continue }
        $TokenMap[$p.Name] = [string]$p.Value
    }
}

function Resolve-Token {
    param([string]$AgentKey)
    if ($TokenMap.ContainsKey($AgentKey) -and -not [string]::IsNullOrWhiteSpace($TokenMap[$AgentKey])) {
        return $TokenMap[$AgentKey]
    }
    if (-not [string]::IsNullOrWhiteSpace($DefaultToken)) { return $DefaultToken }
    return '<YOUR_TOKEN>'
}

# ---------------------------------------------------------------------------
# 2. 生成
# ---------------------------------------------------------------------------
if (-not (Test-Path -LiteralPath $OutDir)) {
    New-Item -ItemType Directory -Path $OutDir -Force | Out-Null
}
$OutDir = (Resolve-Path -LiteralPath $OutDir).Path

# 防御：绝不允许把 OutDir 指到某个 Agent 的配置目录里
$Forbidden = @(
    (Join-Path $env:USERPROFILE '.dsh'),
    (Join-Path $env:USERPROFILE '.workbuddy'),
    (Join-Path $env:USERPROFILE '.box-agent'),
    (Join-Path $env:USERPROFILE '.codearts'),
    (Join-Path $env:APPDATA 'Trae CN'),
    (Join-Path $env:APPDATA 'TRAE SOLO CN')
)
foreach ($f in $Forbidden) {
    if ($f -and $OutDir.StartsWith($f, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "OutDir 落在 Agent 配置目录内（$f）。本脚本只产出待粘贴文件，不直接改用户配置。"
    }
}

# -File 方式调用时 `-Agents a,b` 会被当成单个字符串，这里统一拆开
if ($Agents -and $Agents.Count -gt 0) {
    $Agents = @($Agents | ForEach-Object { $_ -split ',' } | ForEach-Object { $_.Trim() } | Where-Object { $_ })
}

$selected = $Map
if ($Agents -and $Agents.Count -gt 0) {
    $selected = $Map | Where-Object { $Agents -contains $_.Agent }
    if (-not $selected -or $selected.Count -eq 0) {
        $known = ($Map | ForEach-Object { $_.Agent }) -join ', '
        throw "未匹配到任何 agent：$($Agents -join ', ')。可用：$known"
    }
}

function Convert-ForTemplate {
    param([string]$Value, [bool]$IsJson)
    if (-not $IsJson) { return $Value }
    return $Value.Replace('\', '\\').Replace('"', '\"')
}

$Utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$rows = @()
$problems = @()

foreach ($m in $selected) {
    $tplPath = Join-Path $TemplateDir $m.Template
    if (-not (Test-Path -LiteralPath $tplPath)) {
        $problems += "模板缺失：$($m.Template)"
        continue
    }

    $text = Get-Content -LiteralPath $tplPath -Raw -Encoding UTF8
    $needsToken = $text.Contains('<YOUR_TOKEN>')
    $token = Resolve-Token -AgentKey $m.TokenKey

    # JSON 模板里的值必须做 JSON 字符串转义：
    # Windows 路径（C:\Python314\...）若原样塞进 JSON，`\P` 是非法转义，
    # 解析器直接报错——这是本生成器第一版踩到的真实坑。
    $isJson = $m.Output.ToLowerInvariant().EndsWith('.json')

    # 顺序：先长后短，避免 18443 被 18444 的前缀替换误伤
    $text = $text.Replace('<YOUR_TOKEN>', (Convert-ForTemplate $token $isJson))
    $text = $text.Replace('192.168.0.88', (Convert-ForTemplate $McpHost $isJson))
    $text = $text.Replace('18444', [string]$McpPort)
    $text = $text.Replace('18443', [string]$RestPort)
    $text = $text.Replace('<LIGHTMEM_MCP_BIN>', (Convert-ForTemplate $McpBin $isJson))
    $text = $text.Replace('<LIGHTMEM_PROFILE>', (Convert-ForTemplate $ProfilePath $isJson))
    $text = $text.Replace('<PYTHON_EXE>', (Convert-ForTemplate $PythonExe $isJson))
    $text = $text.Replace('<BRIDGE_PY>', (Convert-ForTemplate $BridgePy $isJson))
    $text = $text.Replace('<TENANT_ID>', (Convert-ForTemplate $TenantId $isJson))
    $text = $text.Replace('<USER_ID>', (Convert-ForTemplate $m.UserId $isJson))
    $text = $text.Replace('<AGENT_ID>', (Convert-ForTemplate $m.AgentId $isJson))

    # JSON 模板生成后必须仍是合法 JSON（防止替换把引号/转义弄坏）
    if ($isJson) {
        try {
            $null = $text | ConvertFrom-Json
        }
        catch {
            $problems += "生成的 JSON 非法（$($m.Output)）：$($_.Exception.Message)"
            continue
        }
    }

    $outPath = Join-Path $OutDir $m.Output
    if ((Test-Path -LiteralPath $outPath) -and -not $Force) {
        $problems += "目标已存在，跳过（加 -Force 覆盖）：$($m.Output)"
        continue
    }
    [System.IO.File]::WriteAllText($outPath, $text, $Utf8NoBom)

    $leftover = ([regex]::Matches($text, '<[A-Z_]+>') | ForEach-Object { $_.Value } | Select-Object -Unique) -join ' '
    $rows += [pscustomobject]@{
        Agent     = $m.Agent
        Transport = $m.Transport
        Output    = $m.Output
        Token     = $(if (-not $needsToken) { '不适用' } elseif ($token -eq '<YOUR_TOKEN>') { '未填' } else { '已填' })
        Leftover  = $(if ([string]::IsNullOrWhiteSpace($leftover)) { '-' } else { $leftover })
    }
}

# ---------------------------------------------------------------------------
# 3. MANIFEST.md —— 生成文件搬到哪、怎么粘
# ---------------------------------------------------------------------------
$sb = New-Object System.Text.StringBuilder
[void]$sb.AppendLine('# 生成结果清单（gen_configs.ps1 自动产出）')
[void]$sb.AppendLine('')
[void]$sb.AppendLine("生成时间：$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')　｜　MCP 端点：http://${McpHost}:${McpPort}/mcp")
[void]$sb.AppendLine('')
[void]$sb.AppendLine('| 生成文件 | Agent | 传输 | 目标配置路径 | 粘贴方式 |')
[void]$sb.AppendLine('|---|---|---|---|---|')
foreach ($m in $selected) {
    [void]$sb.AppendLine("| ``$($m.Output)`` | $($m.Agent) | $($m.Transport) | ``$($m.Target)`` | $($m.Mode) |")
}
[void]$sb.AppendLine('')
[void]$sb.AppendLine('> 本目录里的文件是**待粘贴副本**，脚本没有写入任何 Agent 配置目录。')
[void]$sb.AppendLine('> 逐家验证步骤见 `../VERIFY.md`。')
[System.IO.File]::WriteAllText((Join-Path $OutDir 'MANIFEST.md'), $sb.ToString(), $Utf8NoBom)

# ---------------------------------------------------------------------------
# 4. 汇总
# ---------------------------------------------------------------------------
Write-Host ''
Write-Host "输出目录：$OutDir" -ForegroundColor Cyan
if ($rows.Count -gt 0) {
    $rows | Format-Table -AutoSize | Out-String -Width 200 | Write-Host
}
if ($problems.Count -gt 0) {
    Write-Host '需要处理：' -ForegroundColor Yellow
    foreach ($p in $problems) { Write-Host "  - $p" -ForegroundColor Yellow }
}
if ($rows.Count -eq 0 -and $problems.Count -eq 0) {
    Write-Host '没有生成任何文件（未选中 agent？用 -ListAgents 查看）。' -ForegroundColor Yellow
}

Write-Host ''
Write-Host '提醒：' -ForegroundColor DarkGray
Write-Host '  1) token 仍是 <YOUR_TOKEN> 的模板可直接粘贴，但 Agent 会连上却写不进（401）。' -ForegroundColor DarkGray
Write-Host '  2) 18444 由 A 线原生 MCP 提供；未上线时先按 README §8 用 18443 过渡通道。' -ForegroundColor DarkGray
Write-Host '  3) 本脚本不改用户配置、不启动服务、不联网。' -ForegroundColor DarkGray
