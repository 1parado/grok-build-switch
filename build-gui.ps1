[CmdletBinding()]
param(
	[string]$Version = $env:GROK_SWITCH_VERSION,
  [string]$CertificatePath = $env:GROK_SWITCH_SIGN_CERT,
  [string]$CertificatePassword = $env:GROK_SWITCH_SIGN_PASSWORD,
  [string]$CertificateThumbprint = $env:GROK_SWITCH_SIGN_THUMBPRINT,
  [string]$TimestampUrl = "http://timestamp.digicert.com",
  [switch]$RequireSignature
)

$ErrorActionPreference = "Stop"
$PSNativeCommandUseErrorActionPreference = $true

function Get-CodeSigningCertificate {
  if ($CertificatePath) {
    $resolvedCertificate = (Resolve-Path -LiteralPath $CertificatePath).Path
    if ($CertificatePassword) {
      $securePassword = ConvertTo-SecureString $CertificatePassword -AsPlainText -Force
      return Get-PfxCertificate -FilePath $resolvedCertificate -Password $securePassword
    }
    return Get-PfxCertificate -FilePath $resolvedCertificate
  }

  if ($CertificateThumbprint) {
    $normalizedThumbprint = ($CertificateThumbprint -replace '\s', '').ToUpperInvariant()
    foreach ($store in @("Cert:\CurrentUser\My", "Cert:\LocalMachine\My")) {
      $candidate = Join-Path $store $normalizedThumbprint
      if (Test-Path -LiteralPath $candidate) {
        return Get-Item -LiteralPath $candidate
      }
    }
    throw "Code-signing certificate thumbprint not found: $CertificateThumbprint"
  }

  return $null
}

function Sign-Executable([string]$ExecutablePath) {
  $certificate = Get-CodeSigningCertificate
  $requireFromEnvironment = $env:GROK_SWITCH_REQUIRE_SIGNATURE -match '^(1|true|yes)$'
  if (-not $certificate) {
    if ($RequireSignature -or $requireFromEnvironment) {
      throw "A release signature is required, but no certificate was configured."
    }
    Write-Warning "No signing certificate configured; this is an unsigned Wails GUI development build."
    return $false
  }

  if (-not $certificate.HasPrivateKey) {
    throw "The configured certificate does not contain a private key."
  }
  $signature = Set-AuthenticodeSignature `
    -FilePath $ExecutablePath `
    -Certificate $certificate `
    -HashAlgorithm SHA256 `
    -TimestampServer $TimestampUrl
  if ($signature.Status -ne [System.Management.Automation.SignatureStatus]::Valid) {
    throw "Authenticode signing failed: $($signature.Status) $($signature.StatusMessage)"
  }
  Write-Host "Signed $ExecutablePath with $($certificate.Subject)"
  return $true
}

Push-Location $PSScriptRoot
try {
	if (-not $Version) {
		$PSNativeCommandUseErrorActionPreference = $false
		$Version = (git describe --tags --exact-match 2>$null)
		$PSNativeCommandUseErrorActionPreference = $true
		if ($LASTEXITCODE -ne 0 -or -not $Version) { $Version = "dev" }
	}
	if ($Version -ne "dev" -and $Version -notmatch '^v?\d+(\.\d+)+([+-][0-9A-Za-z.-]+)?$') {
		throw "Invalid build version: $Version"
	}
  go test ./...
  go test -tags "wailsgui,desktop,production" .

  $resourceOutput = ".\rsrc_windows_amd64.syso"
  $rsrcCommand = Get-Command rsrc -ErrorAction SilentlyContinue
  $rsrcPath = if ($rsrcCommand) { $rsrcCommand.Source } else { Join-Path (go env GOPATH) "bin\rsrc.exe" }
  if ((Test-Path -LiteralPath $rsrcPath) -and -not (Test-Path -LiteralPath $resourceOutput)) {
    & $rsrcPath -ico ".\assets\icon.ico" -o $resourceOutput
  }

	go build -tags "wailsgui,desktop,production" -trimpath -ldflags "-s -w -H windowsgui -X main.version=$Version" -o grok_switch_gui.exe .
  $executable = Join-Path $PSScriptRoot "grok_switch_gui.exe"
  [void](Sign-Executable $executable)
  $hash = (Get-FileHash -LiteralPath $executable -Algorithm SHA256).Hash.ToLowerInvariant()
  $checksumPath = "$executable.sha256"
  Set-Content -LiteralPath $checksumPath -Value "$hash  grok_switch_gui.exe" -Encoding ascii
  Write-Host "Built $executable"
	Write-Host "Version $Version"
  Write-Host "SHA-256 $checksumPath"
} finally {
  Pop-Location
}
