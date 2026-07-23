[CmdletBinding()]
param(
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
    Write-Warning "No signing certificate configured; this is an unsigned development build."
    return $false
  }

  if (-not $certificate.HasPrivateKey) {
    throw "The configured certificate does not contain a private key."
  }
  $usage = @($certificate.Extensions | Where-Object { $_.Oid.Value -eq '2.5.29.37' } | ForEach-Object { $_.Format($false) }) -join ' '
  if ($usage -and $usage -notmatch 'Code Signing|1\.3\.6\.1\.5\.5\.7\.3\.3') {
    throw "The configured certificate is not valid for code signing."
  }

  $signature = Set-AuthenticodeSignature `
    -FilePath $ExecutablePath `
    -Certificate $certificate `
    -HashAlgorithm SHA256 `
    -TimestampServer $TimestampUrl
  if ($signature.Status -ne [System.Management.Automation.SignatureStatus]::Valid) {
    throw "Authenticode signing failed: $($signature.Status) $($signature.StatusMessage)"
  }

  $verified = Get-AuthenticodeSignature -FilePath $ExecutablePath
  if ($verified.Status -ne [System.Management.Automation.SignatureStatus]::Valid) {
    throw "Authenticode verification failed: $($verified.Status) $($verified.StatusMessage)"
  }
  Write-Host "Signed $ExecutablePath with $($certificate.Subject)"
  return $true
}

Push-Location $PSScriptRoot
try {
  go test ./...
  if (Get-Command magick -ErrorAction SilentlyContinue) {
    $temporaryIcon = Join-Path $env:TEMP ("grok-switch-icon-" + [guid]::NewGuid().ToString("N") + ".ico")
    try {
      magick ".\icon.svg" -background none -define icon:auto-resize=256,128,64,48,32,16 $temporaryIcon
      if (-not (Test-Path -LiteralPath $temporaryIcon)) {
        throw "ImageMagick did not produce an icon file."
      }
      Move-Item -LiteralPath $temporaryIcon -Destination ".\assets\icon.ico" -Force
    } catch {
      if (-not (Test-Path -LiteralPath ".\assets\icon.ico")) {
        throw
      }
      Write-Warning "ImageMagick icon generation failed; using existing assets\icon.ico. $($_.Exception.Message)"
    } finally {
      Remove-Item -LiteralPath $temporaryIcon -Force -ErrorAction SilentlyContinue
    }
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
  $resourceOutput = ".\rsrc_windows_amd64.syso"
  if ($rsrcPath) {
    $temporaryResource = Join-Path $env:TEMP ("grok-switch-resource-" + [guid]::NewGuid().ToString("N") + ".syso")
    try {
      & $rsrcPath -ico ".\assets\icon.ico" -o $temporaryResource
      if (-not (Test-Path -LiteralPath $temporaryResource)) {
        throw "rsrc did not produce a resource file."
      }
      Move-Item -LiteralPath $temporaryResource -Destination $resourceOutput -Force
    } catch {
      Remove-Item -LiteralPath $resourceOutput -Force -ErrorAction SilentlyContinue
      Write-Warning "rsrc generation failed; building without an Explorer executable icon. $($_.Exception.Message)"
    } finally {
      Remove-Item -LiteralPath $temporaryResource -Force -ErrorAction SilentlyContinue
    }
  } else {
    Remove-Item -LiteralPath $resourceOutput -Force -ErrorAction SilentlyContinue
    Write-Host "rsrc not found; building without embedded exe icon"
  }
  go build -ldflags "-s -w -H windowsgui" -o grok_switch.exe .
  $executable = Join-Path $PSScriptRoot "grok_switch.exe"
  [void](Sign-Executable $executable)
  $hash = (Get-FileHash -LiteralPath $executable -Algorithm SHA256).Hash.ToLowerInvariant()
  $checksumPath = "$executable.sha256"
  Set-Content -LiteralPath $checksumPath -Value "$hash  grok_switch.exe" -Encoding ascii
  Write-Host "Built $executable"
  Write-Host "SHA-256 $checksumPath"
} finally {
  Pop-Location
}
