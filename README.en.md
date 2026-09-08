# Grok_switch

[简体中文](./README.md) | **English**

<img width="188" height="66" alt="image" src="https://github.com/user-attachments/assets/f2ee6b24-6a15-4912-a6ed-67f3ffb6c1a4" />

A local tray tool that manages Grok CLI's `~/.grok/config.toml` through provider profiles.

Switch the upstream `base_url`, default model, web-search model, subagents and every `[model.*]` definition with one click.

## Features

- Provider CRUD: name, Base URL, API key, upstream format, default / web-search / explore·plan subagent models, enabled model list

  <img width="1524" height="1012" alt="image" src="https://github.com/user-attachments/assets/cdcfddd0-55d4-4ae5-a830-4f99c1525b9b" />

- Providers use `high` reasoning effort by default; regular models automatically get `supports_reasoning_effort = true` and the `low/medium/high` effort list
- `/imagine` / `image_gen` can use a dedicated image service URL, API key, backend protocol and model; supports fetching models independently and real generation tests. It does not inherit language-model credentials and never adds image models to the chat model list. **Grok CLI's ImageGen uses the chat session key against `xai_api_base_url`**, so when standalone image generation is enabled, grok_switch points `xai_api_base_url` at a local proxy `http://127.0.0.1:<port>/imagine/v1` and injects your configured image key (grok_switch must stay running)
- One-click activation: writes `[endpoints]`, `[models]`, `[subagents.models]` (explore / plan) and `[model.*]`, preserving all other sections as much as possible
- Automatic backup before switching or saving config; restore backups or edit `config.toml` directly from the Settings page
- First run can import a Default provider from your current `config.toml`
- Import CPA `xai-*.json` or Grok CLI `auth.json`; an embedded proxy provides a stable local URL/key and refreshes tokens automatically

  <img width="1240" height="544" alt="image" src="https://github.com/user-attachments/assets/87625b01-f77f-464b-8335-61a7401ba9be" />

- Grok multi-account pool: batch import, scheduled auto-inspection, health classification, automatic quarantine of bad accounts, rotation of healthy accounts and single-account fallback
- Built-in Grok Build AI chat workspace: streaming replies, tool permissions, history session resume and working-directory selection
- **Global switch** for image generation (Settings / provider form): when on, models can generate images via the MCP tool and built-in image_gen (account pool, no subscription needed); when off, MCP registration and tool injection are removed and models return to their original no-image state — effective immediately
- Image generation as an Agent-native MCP tool: registered through Grok CLI's native `[mcp_servers]` (loaded by Grok CLI itself). In chat, the model can call `generate_image` itself, write its own prompts, and choose the mode (speed / standard / quality) and aspect ratio (1:1, 16:9, 9:16, 4:3, 3:4, 3:2, 2:3, 4:5, 21:9, etc.). Results come back inline; the same MCP server works with any MCP-capable harness
- OpenAI-compatible harnesses can generate images too: the local proxy automatically injects the `generate_image` tool declaration into `chat/completions` requests, executes the call for the model (via the account pool) and returns the image inline (base64). Works out of the box on any function-calling platform
- Built-in `image_gen` automatically uses the account pool: under the Grok Auth local proxy, the model's built-in image-tool requests are taken over by the proxy and fulfilled by the account pool — works even when the official account has no quota (no subscription needed)
- AI-native rich-text replies: GFM, code highlighting & copy, Mermaid, KaTeX, images and citations
- Web UI listens on `127.0.0.1` by default; enabling LAN access switches to `0.0.0.0` (default port `17878`)
- Optional LAN phone access: pair via QR code on the same network to manage providers or continue AI conversations

  <img width="1375" height="962" alt="image" src="https://github.com/user-attachments/assets/5d106a9e-f5bc-4f0b-a0fa-8b1473c544c8" />

- Optional Windows launch-at-login
- Single-instance on Windows; double-clicking the EXE again opens the already-running instance's management page
- Checks for stable releases 15 seconds after launch, then every 24 hours; a new version shows a desktop notification with one-click download, and you can skip a version
- Native error dialog on startup failure, with diagnostic logs whenever possible
- Tray menu: quick switching, open panel, copy address, open data/log directories

## Requirements

| Item | Notes |
|------|-------|
| OS | **Windows 10 / 11 x64**, **macOS (Intel / Apple Silicon)**, **Linux (amd64 / arm64)** |
| Runtime | Download the binary/archive for your platform and run it — **no** Go / Node required |
| Optional | [Grok CLI](https://x.ai) installed locally; config directory defaults to `~/.grok` (`%USERPROFILE%\.grok` on Windows) |

## Install & Usage

### Online docs

Tutorials, screenshots and contact info are on the project docs site:

[https://1parado.github.io/grok-build-switch/](https://1parado.github.io/grok-build-switch/)

### Option 1: Download from Releases (recommended)

**[One-click download of the latest Windows tray EXE](https://github.com/1parado/grok-build-switch/releases/latest/download/grok_switch.exe)**

[Download the Windows GUI EXE](https://github.com/1parado/grok-build-switch/releases/latest/download/grok_switch_gui.exe) · [All versions & release notes](https://github.com/1parado/grok-build-switch/releases/latest)

Pick the package for your OS and architecture:

| OS | Arch | Artifact name | Notes |
|---|---|---|---|
| **Windows** | `amd64` (x64) | `grok_switch.exe` / `grok_switch_gui.exe` (or `grok_switch_windows_amd64.exe`) | Includes CLI and GUI EXE |
| **macOS** | `amd64` (Intel) | `grok_switch_darwin_amd64.tar.gz` | Archive includes CLI and GUI |
| **macOS** | `arm64` (Apple Silicon) | `grok_switch_darwin_arm64.tar.gz` | Archive includes CLI and GUI |
| **Linux** | `amd64` (x86_64) | `grok_switch_linux_amd64.tar.gz` | Archive includes CLI and GUI |
| **Linux** | `arm64` (aarch64) | `grok_switch_linux_arm64.tar.gz` | Archive includes CLI and GUI |

1. Use the direct link above or grab the archive/binary for your platform from Releases
2. Extract and run (double-click the `.exe` on Windows; on macOS / Linux extract and run `./grok_switch` in a terminal)
3. A tray icon appears; the browser opens `http://127.0.0.1:17878/` (can be disabled in Settings via "Open panel on launch")
5. Double-clicking the EXE again does not start a second background instance — it opens the already-running management page

Regular users just download and run — **no** certificates, signing passwords, Go or Node needed. The release pipeline signs automatically when a code-signing certificate is configured; unsigned builds still run but Windows may show a SmartScreen prompt. The signing configuration below is only for release maintainers.

### Option 2: Build from source

See [Build](#build) below.

### Daily use

1. **Add a provider**: "Add provider" at the top → fill in name, URL, API key → optionally expand "Connection & models" to fetch and enable models → **Save & activate**
2. **Switch upstream**: click **Activate** on the target provider card
3. **View the live config**: Settings → `config.toml` editor (only one live config on disk; provider profiles live locally)
4. **Note**: switching does **not** terminate running grok sessions; only **new** grok sessions read the new config

### Phone/tablet access & AI chat

1. On the computer, enable **Settings → Allow phone access on the same LAN** and save.
2. Make sure the phone/tablet and the computer are on the same Wi-Fi or hotspot; scan the QR code in the "Phone pairing" section.
3. After one-time pairing, the phone opens the same page where you can manage providers, view chat history and continue using Grok Build. If the browser loses the pairing cookie, it returns to the pairing page instead of showing a server error.
4. Pairing codes are valid for 10 minutes and single-use; to re-authorize, click "Regenerate QR code" on the computer.
5. On the phone chat page, tap "···" in the top-right to open session info, enter the working directory on the computer and start/stop the Agent; tapping the scrim outside the sidebar closes it.

Chat requires the computer running the EXE to have Grok Build installed and signed in, with `grok` executable in the terminal. Tool calls, commands and file reads/writes always happen on the computer; the phone is only a paired control surface.

LAN access is off by default. When enabled, use it only on trusted home or office networks; in Windows Firewall only allow `grok_switch` on "Private networks" and never forward the port to the public internet. For cross-network access use a VPN like Tailscale, ZeroTier or WireGuard.

<img width="1067" height="1944" alt="image" src="https://github.com/user-attachments/assets/5d106a9e-f5bc-4f0b-a0fa-8b1473c544c8" />

### Chat theme background

The `◐` button in the chat workspace's top-right switches between Pure, Frost, Deep Space and Afterglow backgrounds, or imports a local PNG/JPEG/WebP image. Backgrounds support shade strength, blur and horizontal/vertical focus.

Themes are per-device appearance preferences stored in the browser/WebView2 local storage — they are never written into Grok sessions, API configuration or synced to paired phones. Custom images are compressed and stored locally; the original file is never moved or modified.

<img width="1900" height="975" alt="image" src="https://github.com/user-attachments/assets/b75afab5-681a-4316-804c-44ad1fe81103" />

### Grok Auth & the account pool

1. Open **Settings → Grok Auth JSON** to import a single CPA `xai-*.json` or `%USERPROFILE%\.grok\auth.json`; this entry shares the same pool as auto-inspection below.
2. For multiple accounts, pick several JSON files at once in **Grok pool auto-inspection**, or use "Import folder" to recursively read all `.json` files in a directory and its subdirectories; original files are never moved.
3. By default, accounts are inspected right after import, then every 6 hours; the interval is adjustable from 30–1440 minutes with 1–16 concurrent workers.
4. Once inspection confirms permission denied, free quota exhausted or auth invalid, the account leaves the proxy-available set; ordinary 429/network errors are not mis-quarantined.
5. Account cards show HTTP status, error codes and the concrete probe error; you can batch-disable or batch-delete all "inspected and unhealthy" abnormal accounts — pending accounts are left untouched.
6. Auto-inspection never deletes accounts. Manual disable, enable, single delete and batch operations are all done on the Accounts page.
7. Without direct access to xAI, set an HTTP/HTTPS/SOCKS5 proxy such as `http://127.0.0.1:7890`; this setting is shared by inspection, token refresh and pool live forwarding.

### Environment variables

| Variable | Notes |
|------|-------|
| `GROK_CONFIG` | Full path to `config.toml` |
| `GROK_HOME` | Path to the `.grok` directory, default `%USERPROFILE%\.grok` |

## Data & Security

### Where the data lives

| Path | Contents |
|------|------|
| `%USERPROFILE%\.grok\config.toml` | Grok CLI's **live** config |
| `%USERPROFILE%\.grok_switch\profiles.json` | Provider profiles (**plaintext API keys**) |
| `%USERPROFILE%\.grok_switch\backups\` | Automatic config backups (**contain keys**) |
| `%USERPROFILE%\.grok_switch\settings.json` | This tool's settings |
| `%USERPROFILE%\.grok_switch\remote_access.json` | LAN phone sessions and one-time pairing credentials (**sensitive**) |
| `%USERPROFILE%\.grok_switch\grok_auth.json` | Single-account xAI OAuth credentials and local proxy key (**sensitive**) |
| `%USERPROFILE%\.grok_switch\grok_pool\pool.json` | Pool display state and inspection/proxy settings (no tokens; proxy URL may contain auth info) |
| `%USERPROFILE%\.grok_switch\grok_pool\accounts\` | Per-account OAuth credential copies (**sensitive**) |
| `%USERPROFILE%\.grok_switch\registrar\cookies\` | Registrar browser cookie snapshots (**sensitive, can restore login state**) |
| `%USERPROFILE%\.grok_switch\grok_switch.log` | Logs |
| `%USERPROFILE%\.grok_switch\update_state.json` | Update notification and skipped-version state |

If a persisted JSON file is corrupted by power loss, manual editing or sync-tool conflicts, the program first renames the original to
`<filename>.corrupt-<timestamp>.bak`, then restores safe defaults. The recovery is written to `grok_switch.log`;
corrupted originals are never silently overwritten.

## Build

### Environment

- [Go](https://go.dev/dl/) **1.26+** (match `go.mod`)
- Windows x64
- Optional: `rsrc` (embed exe icon), ImageMagick `magick` (generate ico from svg)

```powershell
# Optional: embed icon resources
go install github.com/akavel/rsrc@latest
```

### One-click build

```powershell
.\build.ps1
```

Runs tests and produces `grok_switch.exe`.

### Wails desktop GUI (parallel edition)

The project also ships an independent Wails v2 desktop build that does not replace the tray edition:

```powershell
.\build-gui.ps1
```

This produces `grok_switch_gui.exe` and `grok_switch_gui.exe.sha256`. The two editions:

| File | How it runs |
|------|-------------|
| `grok_switch.exe` | Classic tray + browser management UI |
| `grok_switch_gui.exe` | Wails/WebView2 native window; closing the window hides it to the system tray |

The GUI edition reuses the same Go services, Web UI, configuration and LAN phone access. The tray "Providers" submenu shows the current provider and quick-switches between the official account or any profile; the tray syncs automatically when providers change in the GUI/Web page. Clicking the window close button only hides the window — background services keep running; restore it via "Open GUI window" in the tray menu, and only "Quit grok_switch GUI" fully exits the GUI and its own services. If the tray edition is already running, the GUI connects to the existing local service and does not start the Agent, proxy or HTTP listener again. Windows 10/11 usually ships with the WebView2 Runtime; if missing, the GUI shows an installation prompt.

The following only applies to maintainers building from source or publishing Releases; Release users can skip it.

Without a certificate, local builds clearly report an unsigned dev build and also produce
`grok_switch.exe.sha256`. Official releases should be signed with Authenticode:

```powershell
$env:GROK_SWITCH_SIGN_CERT = "C:\secure\grok-switch-signing.pfx"
$env:GROK_SWITCH_SIGN_PASSWORD = "<pfx-password>"
.\build.ps1 -RequireSignature
```

You can also sign with a thumbprint from the current-user or local-machine certificate store:

```powershell
$env:GROK_SWITCH_SIGN_THUMBPRINT = "<certificate-thumbprint>"
.\build.ps1 -RequireSignature
```

The repo's `Windows Release` workflow injects the version number, builds and uploads fixed-name
EXE and SHA-256 files when a `v*` tag is pushed. Fixed names keep the `releases/latest/download/...`
links always pointing at the latest version. With the following GitHub Actions Secrets configured,
Authenticode signing is enforced; without them, the workflow publishes an unsigned build with a clear
warning and does not block the Release:

- `WINDOWS_SIGNING_CERT_BASE64`: Base64 content of the PFX file
- `WINDOWS_SIGNING_CERT_PASSWORD`: PFX password

Never commit the certificate private key or password to the repo.

### Manual build

```powershell
go test ./...
go build -ldflags "-s -w -H windowsgui" -o grok_switch.exe .
```

- `-H windowsgui`: no console window
- `-s -w`: smaller binary

## Development

```powershell
go test ./...
go run . -no-tray   # HTTP only, no tray (debugging)
```

### Local docs preview

The docs site uses MkDocs Material; content lives in `docs/`:

```powershell
uvx --with mkdocs-material --with mkdocs-static-i18n mkdocs serve
```

Open the local address printed in the terminal. After pushing to `main`, GitHub Actions automatically publishes to GitHub Pages.

Main directories:

```
main.go           # entry point
internal/         # config IO, providers, HTTP, tray
ui/               # Web frontend (embedded into the exe)
assets/           # icons
docs/             # MkDocs site
```

##

[Usage tutorial](https://1parado.github.io/grok-build-switch/)

## Feedback group

Join the grok build switch feedback group:

<img src="./QQ.jpg" alt="grok build switch feedback group QR code" width="360">

## License

[MIT](./LICENSE)

## Friends

Learn AI on L!
[L forum](https://linux.do/)
