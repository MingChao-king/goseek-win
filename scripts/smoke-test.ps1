# GoSeek Windows 版离线自检（PowerShell 5.1+，无需管理员）。
#
# 不需要真实 API key 或中转：脚本在本地启动 mock-provider（假的 OpenAI-compatible
# 流式供应商），再启动 goseek.exe serve，提交一条消息，验证完整链路：
#   会话创建 → 流式模型请求 → 工具调用（write_file 真实写盘）→ 观察回填 → 文本回复
#
# 用法（在 PowerShell 里）：
#   cd 解压后的目录
#   powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1 `
#       -GoseekBin .\goseek.exe -MockBin .\mock-provider.exe
#
# 参数：
#   -GoseekBin  goseek.exe 的路径（安装包里自带）
#   -MockBin    mock-provider.exe 的路径（安装包里自带）
#   -ServePort  被测服务端口，默认 18765（刻意避开默认的 8765，不打扰你在跑的实例）
#   -MockPort   mock 供应商端口，默认 19876
param(
  [string]$GoseekBin = "",
  [string]$MockBin = "",
  [int]$ServePort = 18765,
  [int]$MockPort = 19876
)

$ErrorActionPreference = "Stop"

# 找二进制：优先参数，其次脚本同级的仓库布局（backend\release\）。
if (-not $GoseekBin -or -not (Test-Path $GoseekBin)) {
  $candidate = Join-Path $PSScriptRoot "..\backend\release\goseek.exe"
  if (Test-Path $candidate) { $GoseekBin = (Resolve-Path $candidate).Path }
}
if (-not $MockBin -or -not (Test-Path $MockBin)) {
  $candidate = Join-Path $PSScriptRoot "..\backend\release\mock-provider.exe"
  if (Test-Path $candidate) { $MockBin = (Resolve-Path $candidate).Path }
}
if (-not ($GoseekBin -and (Test-Path $GoseekBin))) {
  Write-Host "!! 找不到 goseek.exe：用 -GoseekBin 指定，或先按 README 构建后再运行"; exit 1
}
if (-not ($MockBin -and (Test-Path $MockBin))) {
  Write-Host "!! 找不到 mock-provider.exe：用 -MockBin 指定（安装包里自带）"; exit 1
}

$work = Join-Path ([System.IO.Path]::GetTempPath()) ("goseek-smoke-" + [guid]::NewGuid().ToString("N").Substring(0,8))
New-Item -ItemType Directory -Path $work | Out-Null
New-Item -ItemType Directory -Path (Join-Path $work "workspace") | Out-Null
$serverProc = $null; $mockProc = $null

function Cleanup {
  if ($serverProc) { try { $serverProc.Kill() } catch {} }
  if ($mockProc)   { try { $mockProc.Kill() } catch {} }
  Start-Sleep -Milliseconds 300
  try { Remove-Item -Recurse -Force $work } catch {}
}

Write-Host "==> 启动 mock 供应商（127.0.0.1:$MockPort）"
$env:MOCK_PORT = "$MockPort"
$mockProc = Start-Process -FilePath $MockBin -ArgumentList "" `
  -RedirectStandardOutput (Join-Path $work "mock.log") `
  -RedirectStandardError  (Join-Path $work "mock.err") `
  -PassThru -WindowStyle Hidden

Write-Host "==> 启动被测服务（127.0.0.1:$ServePort，数据目录 $work）"
# Windows 版把数据放在 %LOCALAPPDATA%\goseek——测试时把 LOCALAPPDATA 指到临时目录，
# 结束后随临时目录一起删掉，不污染真实数据。
$savedLocalAppData = $env:LOCALAPPDATA
$env:LOCALAPPDATA = $work
$env:GOSEEK_API_KEY = "smoke-test-not-a-real-key"
$env:GOSEEK_BASE_URL = "http://127.0.0.1:$MockPort"
$env:GOSEEK_MODEL = "mock-model"
$env:GOSEEK_CONTEXT_WINDOW = "128000"
$serverProc = Start-Process -FilePath $GoseekBin -ArgumentList "serve","--addr","127.0.0.1:$ServePort" `
  -RedirectStandardOutput (Join-Path $work "serve.log") `
  -RedirectStandardError  (Join-Path $work "serve.err") `
  -PassThru -WindowStyle Hidden
# 环境变量还原（子进程已经继承）。
$env:LOCALAPPDATA = $savedLocalAppData
Remove-Item Env:GOSEEK_API_KEY -ErrorAction SilentlyContinue
Remove-Item Env:GOSEEK_BASE_URL -ErrorAction SilentlyContinue
Remove-Item Env:GOSEEK_MODEL -ErrorAction SilentlyContinue
Remove-Item Env:GOSEEK_CONTEXT_WINDOW -ErrorAction SilentlyContinue
Remove-Item Env:MOCK_PORT -ErrorAction SilentlyContinue

try {
  Write-Host "==> 等待服务就绪"
  $ready = $false
  foreach ($i in 1..80) {
    try {
      Invoke-RestMethod -Uri "http://127.0.0.1:$ServePort/api/v1/sessions" -TimeoutSec 2 | Out-Null
      $ready = $true; break
    } catch { Start-Sleep -Milliseconds 300 }
  }
  if (-not $ready) {
    Write-Host "!! 服务未就绪"; Get-Content (Join-Path $work "serve.err") -ErrorAction SilentlyContinue | Select-Object -First 20; exit 1
  }

  Write-Host "==> 创建会话并发送测试消息"
  $session = Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$ServePort/api/v1/sessions" `
    -ContentType "application/json" -Body ('{"workspace":"' + (Join-Path $work "workspace").Replace('\','\\') + '"}')
  Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$ServePort/api/v1/sessions/$($session.id)/turns" `
    -ContentType "application/json" -Body '{"content":"write the test result into result.txt"}' | Out-Null
  Write-Host "    会话 $($session.id)"

  Write-Host "==> 等待本轮完成"
  $done = $false
  foreach ($i in 1..100) {
    $snap = Invoke-RestMethod -Uri "http://127.0.0.1:$ServePort/api/v1/sessions/$($session.id)"
    $assistants = @($snap.messages | Where-Object { $_.role -eq "assistant" })
    if ($assistants.Count -ge 2 -and $assistants[-1].content) { $done = $true; break }
    Start-Sleep -Milliseconds 300
  }
  if (-not $done) { Write-Host "!! 超时：本轮没有完成"; exit 1 }

  Write-Host "==> 断言：写盘内容、消息序列、最终回复"
  $resultPath = Join-Path $work "workspace\result.txt"
  if (-not (Test-Path $resultPath)) { Write-Host "!! result.txt 未写入"; exit 1 }
  # 注：字符串内容断言刻意只用结构性检查——PowerShell 5.1 对无 charset 的 JSON/无 BOM
  # UTF-8 文本有经典编码陷阱，这里不依赖中文匹配。中文链路由 smoke-test.sh（mac/Linux）覆盖。
  $written = [System.IO.File]::ReadAllBytes($resultPath)
  if ($written.Length -eq 0) { Write-Host "!! result.txt 是空的"; exit 1 }
  $roles = @($snap.messages | ForEach-Object { $_.role })
  if (($roles -join ",") -ne "user,assistant,tool,assistant") {
    Write-Host "!! 消息序列不对: $($roles -join ',')"; exit 1
  }
  $lastReply = @($snap.messages | Where-Object { $_.role -eq "assistant" })[-1].content
  if (-not $lastReply) { Write-Host "!! 最终回复为空"; exit 1 }

  Write-Host ""
  Write-Host "PASS: GoSeek Windows 版自检通过（会话创建 → 流式补全 → write_file 真实写盘"
  Write-Host "      → 观察回填 → 文本回复；临时数据目录已自动清理）"
} finally {
  Cleanup
}
