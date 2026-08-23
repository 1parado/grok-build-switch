# grok_build_switch

## Install (SHA-256)

Pin GitHub Release **v0.6.0** and verify `SHA256SUMS`. Website `install.sh` / `install.ps1` abort on mismatch.

https://github.com/LinespottingOrg/GrokBuildRemote-Agents/releases/tag/v0.6.0
https://github.com/LinespottingOrg/GrokBuildRemote-Agents/blob/main/docs/PINNED-INSTALL.md

```
96cef605d3e030ccef99d27ea6240e0d3b668dd045e6b5b9e585c9fd03c6ef23  gbr-agent-darwin-amd64
de7e065ef2cf6877b3b2cd04679a67b627f876337f529247e236204543e4062c  gbr-agent-darwin-arm64
a50a5c41993e6531a3b477eb409ccc845212bf541384dc803061c80657f86719  gbr-agent-linux-amd64
5bfd22c7110234942c4c02ff8154b836d0af45a9422c178a4f52010187d40061  gbr-agent-linux-arm64
f773b89fd31310172b756e0593e0f3b2382b0a3440af2a7d0a8b3073b0c23e27  gbr-agent-windows-amd64.exe
8fb9efcbc7e2ac91c11964944bf0f45e31bb23f4356d9dcb4b305d7cb9b0fe8c  gbr-agent-windows-arm64.exe
```

```bash
VER=v0.6.0
BASE=https://github.com/LinespottingOrg/GrokBuildRemote-Agents/releases/download/$VER
# swap darwin-arm64 for your OS/arch
curl -fsSL -o gbr-agent-darwin-arm64 "$BASE/gbr-agent-darwin-arm64"
curl -fsSL -o SHA256SUMS "$BASE/SHA256SUMS"
shasum -a 256 -c SHA256SUMS --ignore-missing
gbr-agent pair && gbr-agent run
```


`grok_build_switch` 是一个 Windows 本地托盘工具，用来管理 Grok Build 的 `config.toml`。你可以把不同上游、API Key、默认模型、联网搜索模型和 explore/plan 子代理模型保存成供应商 Profile，然后一键切换。

## 快速入口

- [使用教程](usage.md)
- [Build Remote Agent 配对](gbr.md) — 桌面 Grok Build 会话的手机 spectator（`gbr/1`，不是本工具的局域网面板）
- [项目仓库](https://github.com/1parado/grok-build-switch)
- [联系方式](contact.md)

## 它适合做什么

- 管理多个 Grok Build 上游配置
- 在不同供应商、模型和 API Key 之间快速切换
- 自动备份切换前的 `config.toml`
- 通过本地 Web 面板完成配置和编辑
- 使用 Windows 托盘菜单快速打开面板或切换供应商
- 新版本发布后自动提醒，可在更新条中下载或跳过当前版本

## 下载

[一键下载最新版 Windows 托盘版](https://github.com/1parado/grok-build-switch/releases/latest/download/grok_switch.exe)，或前往 [Releases](https://github.com/1parado/grok-build-switch/releases/latest) 查看更新说明和 GUI 版。

## What the phone sees

**Terminal windows** on this PC (machine-wide mailbox). Not headless OpenCode / CodeNomad sidecar / Electron. `:8788` in a sidecar is Bot API JSON, not a transcript.

https://github.com/LinespottingOrg/GrokBuildRemote-Agents/blob/main/docs/WHAT-THE-PHONE-SEES.md
https://grokbuildremote.com/integrations.html
