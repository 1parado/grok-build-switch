$ErrorActionPreference = "Stop"

Push-Location $PSScriptRoot
try {
  go test ./...
  if (Get-Command magick -ErrorAction SilentlyContinue) {
    magick ".\icon.svg" -background none -define icon:auto-resize=256,128,64,48,32,16 ".\assets\icon.ico"
  } else {
    Write-Host "ImageMagick not found; using existing assets\icon.ico"
  }
  $rsrcCommand = Get-Command rsrc -ErrorAction SilentlyContinue
  $rsrcPath = $null
  if ($rsrcCommand) {
    $rsrcPath = $rsrcCommand.Source
  }
  if (-not $rsrcPath) {
    $candidate = Join-Path (go env GOPATH) "bin\rsrc.exe"
    if (Test-Path $candidate) {
      $rsrcPath = $candidate
    }
  }
  if ($rsrcPath) {
    & $rsrcPath -ico ".\assets\icon.ico" -o ".\rsrc_windows_amd64.syso"
  } else {
    Write-Host "rsrc not found; building without embedded exe icon"
  }
  go build -ldflags "-s -w -H windowsgui" -o grok_switch.exe .
  Write-Host "Built $PSScriptRoot\grok_switch.exe"
} finally {
  Pop-Location
}
