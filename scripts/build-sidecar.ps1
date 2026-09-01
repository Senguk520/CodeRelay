$ErrorActionPreference = 'Stop'
$root = Resolve-Path (Join-Path $PSScriptRoot '..')
$sidecar = Join-Path $root 'sidecars\coderelay-proxy'
$hostLine = & rustc -vV | Select-String '^host:'
$targetTriple = $hostLine.ToString().Split(':', 2)[1].Trim()
if ([string]::IsNullOrWhiteSpace($targetTriple)) {
  throw 'Unable to determine the Rust target triple.'
}
$extension = ''
if ($targetTriple -match 'windows') {
  $extension = '.exe'
}
$bin = Join-Path $sidecar "bin\coderelay-proxy-$targetTriple$extension"
New-Item -ItemType Directory -Force (Split-Path $bin) | Out-Null
Push-Location $sidecar
try {
  go mod download
  go build -trimpath -ldflags "-s -w" -o $bin .
} finally {
  Pop-Location
}
Write-Host "Built $bin"