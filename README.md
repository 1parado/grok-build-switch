# grok_switch

本地托盘工具：用供应商（Profile）管理 Grok CLI 的 `~/.grok/config.toml`。

一键切换上游 `base_url`、默认模型、联网搜索模型、subagents 与各 `[model.*]` 定义。

## 功能

- 供应商增删改查：名称、Base URL、API Key、上游格式、默认 / 联网 / Subagents 模型、已启用模型列表
- 一键启用：写入 `[endpoints]`、`[models]`、`[subagents].default_model` 与 `[model.*]`，其它段尽量保留
- 切换 / 保存 config 前自动备份；设置页可还原备份、直接编辑 `config.toml`
- 首次运行可从当前 `config.toml` 导入 Default 供应商
- Web UI 仅监听 `127.0.0.1`（默认端口 `17878`，被占用时自动递增）
- 可设置Windows 开机自启
- 托盘菜单：快速切换、打开面板、复制地址、打开数据/日志目录

## 系统要求

| 项目 | 说明 |
|------|------|
| 系统 | **Windows 10 / 11 x64**（当前主要支持） |
| 运行 | 双击 `grok_switch.exe` 即可，**无需**安装 Go / Node |
| 可选 | 本机已安装 [Grok CLI](https://x.ai)，配置目录默认为 `%USERPROFILE%\.grok` |

## 安装与使用

### 在线文档

使用教程、截图说明和联系方式会整理在项目文档站：

[https://1parado.github.io/grok-build-switch/](https://1parado.github.io/grok-build-switch/)

### 方式一：从 Release 下载（推荐）

1. 打开本仓库的 [Releases](../../releases) 页面
2. 下载 `grok_switch.exe`（或压缩包内的 exe）
3. 放到任意目录，双击运行
4. 托盘出现图标；浏览器会打开 `http://127.0.0.1:17878/`（可在设置中关闭「启动时打开面板」）



### 方式二：从源码构建

见下方 [构建](#构建)。

### 日常操作

1. **添加供应商**：顶部「添加供应商」→ 填名称、地址、API Key → 可选展开「连接与模型」拉取并启用模型 → **保存并启用**
2. **切换上游**：列表中点目标供应商的 **启用**
3. **查看生效配置**：设置 → `config.toml` 编辑区（磁盘上只有一份生效配置；各供应商档案在本地 profiles 中）
4. **说明**：切换后**不会**结束已运行的 grok 会话；**新开**的 grok 会话才会读新 config

### 环境变量

| 变量 | 说明 |
|------|------|
| `GROK_CONFIG` | 指定 `config.toml` 完整路径 |
| `GROK_HOME` | 指定 `.grok` 目录，默认 `%USERPROFILE%\.grok` |

## 数据与安全

### 数据存在哪

| 路径 | 内容 |
|------|------|
| `%USERPROFILE%\.grok\config.toml` | Grok CLI **当前生效**配置 |
| `%USERPROFILE%\.grok_switch\profiles.json` | 供应商档案（**含 API Key 明文**） |
| `%USERPROFILE%\.grok_switch\backups\` | config 自动备份（**含 Key**） |
| `%USERPROFILE%\.grok_switch\settings.json` | 本工具设置 |
| `%USERPROFILE%\.grok_switch\grok_switch.log` | 日志 |

## 构建

### 环境

- [Go](https://go.dev/dl/) **1.22+**（`go.mod` 中版本以仓库为准；建议使用较新的稳定版）
- Windows x64
- 可选：`rsrc`（嵌入 exe 图标）、ImageMagick `magick`（从 svg 生成 ico）

```powershell
# 可选：嵌入图标资源
go install github.com/akavel/rsrc@latest
```

### 一键构建

```powershell
.\build.ps1
```

会运行测试并生成 `grok_switch.exe`。

### 手动构建

```powershell
go test ./...
go build -ldflags "-s -w -H windowsgui" -o grok_switch.exe .
```

- `-H windowsgui`：无控制台黑窗
- `-s -w`：减小体积


## 开发

```powershell
go test ./...
go run . -no-tray   # 仅 HTTP，无托盘（调试用）
```

### 文档站本地预览

文档站使用 MkDocs Material，内容位于 `docs/`：

```powershell
uvx --with mkdocs-material mkdocs serve
```

打开终端输出的本地地址即可预览。提交到 `main` 后，GitHub Actions 会自动发布到 GitHub Pages。

主要目录：

```
main.go           # 入口
internal/         # 配置读写、供应商、HTTP、托盘
ui/               # Web 前端（嵌入 exe）
assets/           # 图标
docs/             # MkDocs 文档站
```

##

[使用教程](https://1parado.github.io/grok-build-switch/)

## License

[MIT](./LICENSE)

## 友链
学AI 上L站！
[L站链接](https://linux.do/)
