$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$projectRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$distDir = Join-Path $projectRoot "dist"

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "Go não foi encontrado no PATH."
}

New-Item -ItemType Directory -Force -Path $distDir | Out-Null

$goFiles = Get-ChildItem -Path $projectRoot -Recurse -File -Filter "*.go" |
    ForEach-Object { $_.FullName }
$unformatted = gofmt -l $goFiles
if ($LASTEXITCODE -ne 0) {
    throw "gofmt falhou."
}
if ($unformatted) {
    throw "Existem arquivos Go sem formatação: $($unformatted -join ', ')"
}

Push-Location $projectRoot
try {
    go test ./...
    if ($LASTEXITCODE -ne 0) {
        throw "Os testes falharam."
    }

    $env:CGO_ENABLED = "0"
    $env:GOOS = "windows"
    foreach ($architecture in @("amd64", "386")) {
        $env:GOARCH = $architecture
        $output = Join-Path $distDir "HdRecover-windows-$architecture.exe"
        go build -trimpath -ldflags="-s -w -H=windowsgui" -o $output .
        if ($LASTEXITCODE -ne 0) {
            throw "A compilação para $architecture falhou."
        }
    }

    Get-ChildItem -Path $distDir -Filter "*.exe" |
        Get-FileHash -Algorithm SHA256 |
        ForEach-Object { "$($_.Hash.ToLower())  $([IO.Path]::GetFileName($_.Path))" } |
        Set-Content -Encoding ascii -Path (Join-Path $distDir "SHA256SUMS.txt")
}
finally {
    Pop-Location
}

Write-Host "Build concluído em $distDir"
