# Cross-compile zpilot for all supported platforms into dist\.
$ErrorActionPreference = 'Stop'
$version = if ($env:VERSION) { $env:VERSION } else { (git describe --tags --always 2>$null) }
if (-not $version) { $version = 'dev' }
Set-Location (Join-Path $PSScriptRoot '..')
New-Item -ItemType Directory -Force dist | Out-Null
$targets = 'windows/amd64','windows/arm64','linux/amd64','linux/arm64','darwin/amd64','darwin/arm64'
foreach ($t in $targets) {
  $os, $arch = $t.Split('/')
  $out = "dist/zpilot-$version-$os-$arch" + $(if ($os -eq 'windows') { '.exe' } else { '' })
  Write-Host "building $out"
  $env:CGO_ENABLED = '0'; $env:GOOS = $os; $env:GOARCH = $arch
  go build -trimpath -ldflags "-s -w -X main.version=$version" -o $out ./cmd/zpilot
  if ($LASTEXITCODE -ne 0) { throw "build failed for $t" }
}
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
Get-ChildItem dist -Filter 'zpilot-*' | ForEach-Object { "{0}  {1}" -f (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower(), $_.Name } | Set-Content dist/SHA256SUMS.txt -Encoding ascii
Write-Host 'done: dist\'
