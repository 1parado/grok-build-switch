# xai-relogin — 重新登录 + 重新铸造工具

批量把 registrar 注册过的 x.ai/Grok 账号重新登录一遍，拿到全新 sso 会话后
重新走 CPA 设备码流程，铸造新的 `access_token` / `refresh_token`，并把
cookie 快照、CPA 凭据文件写回原目录。

## 为什么需要它

- 注册时产生的 `sso` cookie 约 **24 小时**过期；
- 关联的 OAuth `refresh_token` 会被服务端**吊销**（实测 17/17 全部
  `invalid_grant: revoked`），`access_token` 仅 6 小时有效；
- 因此旧 cookie / token 无法本地续期，只能**重新登录换全新会话**。

## 构建

```powershell
cd E:\local_switch\grok_switch
go build -o xai-relogin.exe ./cmd/xai-relogin
```

## 用法

```powershell
# 全部账号（串行，每个约 40~60s）
.\xai-relogin.exe

# 只处理前 N 个
.\xai-relogin.exe -limit 10

# 只处理指定邮箱（逗号分隔）
.\xai-relogin.exe -emails a@x.xyz,b@x.xyz

# 覆盖代理 / 浏览器模式
.\xai-relogin.exe -proxy http://127.0.0.1:7897 -browser-mode visible
```

### 参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-config` | `%USERPROFILE%\.grok_switch\registrar.json` | registrar 配置（代理、超时等） |
| `-accounts` | `%USERPROFILE%\.grok_switch\registrar\accounts_cli.txt` | 账号清单（`邮箱----密码----jwt`） |
| `-auth-dir` | `%USERPROFILE%\.grok_switch\cpa_auths` | 新 CPA 凭据输出目录（覆盖同名文件） |
| `-cookie-dir` | `%USERPROFILE%\.grok_switch\registrar\cookies` | 新 cookie 快照输出目录（覆盖同名文件） |
| `-emails` | 空 | 邮箱白名单 |
| `-limit` | 0（全部） | 最大处理数 |
| `-proxy` | 空（用配置） | 覆盖代理地址 |
| `-browser-mode` | 空（用配置） | `visible` / `headless` |
| `-report` | `registrar\relogin-report-<时间戳>.json` | 结果报告路径 |

## 流程（每个账号）

1. 启动可见 Chrome（走 Clash 代理，自动处理 Cloudflare/Turnstile）；
2. 打开 `https://accounts.x.ai/login?redirect=grok-com`；
3. 填邮箱 → 提交 → 填密码 → 提交；
4. 等待全新 `sso` cookie（约 10~20s）；
5. CPA 设备码授权（浏览器真实点击「允许」）→ 换取新 token；
6. 写回 `cpa_auths\xai-<email>.json` 与 `cookies\<email>-<hash>.json`。

## 已知限制

- 若登录要求**邮箱验证码（OTP）**，会直接跳过并标记失败（当前不读临时邮箱收码）；
- 密码错误 / 账号锁定 / 风控被拒会记录到报告；
- 串行处理，全部 58 个号约需 **40~60 分钟**，期间会逐个弹出 Chrome 窗口；
- 新 token 仍只有 6 小时；建议配合应用内 `internal/grokauth` 的自动刷新
  （过期前 5 分钟用 refresh_token 续期）使用，避免到期后再整批重登。