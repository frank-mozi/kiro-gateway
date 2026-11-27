# Kiro Gateway

Kiro Gateway 把 Kiro 企业账号接入本机网关，供 Claude Code CLI 和 Codex CLI 使用。它通过 Kiro 官方组织登录流程认证，默认只监听 `127.0.0.1:20180`，无需 sudo 或 systemd。本项目不是 Kiro 官方产品。

## 快速开始

1. 从 [GitHub Releases](https://github.com/frank-mozi/kiro-gateway/releases) 选择适合平台的 **`kiro-gateway_portable-…`** 完整包：Windows x64 用 ZIP，Linux x64 和 Intel macOS 用 TAR.GZ。不要把供自动更新使用的平台 ZIP 当作首次安装包。
2. 将整个包解压到固定的、仅当前用户可写的目录。Windows 双击 `kiro-gateway.exe`；Linux/macOS 在解压目录运行 `./kiro-gateway`。如果浏览器没有打开，手动访问 <http://127.0.0.1:20180/>。
3. 在控制台运行“初始化 / 检查”，再点“认证”。在 Kiro 页面选 **Your Organization**，填写工作邮箱并完成组织身份认证。
4. 启用模型 API，在“Claude 设置”或“Codex 设置”中选择模型并保存。Claude Code 可用 `/effort` 指定推理强度；未显式指定时沿用 Kiro 默认值。实际可调用的模型取决于账号权限。

**登录状态保存在安装目录的 `data` 中。** 从同一目录重启通常不需要重新登录；另解压到新目录不会自动继承账号。不要公开或分发已运行过的目录，其中可能有数据库、密钥、日志和备份。

**Kiro 企业认证不需要 Google 或 iFlow 凭据。** Gemini CLI、Antigravity 和 iFlow 的独立 OAuth 登录入口默认关闭。如需使用某个入口，请在网关进程的环境中配置自己的 OAuth 应用：分别以 `KIRO_GATEWAY_GEMINI_CLI`、`KIRO_GATEWAY_ANTIGRAVITY` 或 `KIRO_GATEWAY_IFLOW` 为前缀，同时设置 `_CLIENT_ID` 和 `_CLIENT_SECRET`，然后重启。不要把值写进源码或分发包。

## 客户端使用与操作注意事项

### 1. 网关目录与登录持久化（避免重复登录与 401）

- **固定运行目录**：Kiro 企业登录凭据（支持自动续期）、加密主密钥（`master.key`）以及分配给本地客户端的访问密钥都保存在安装目录的 `data/` 下。日常关闭重开请始终运行**同一目录**下的程序，无需重新登录 Kiro。
- **切勿混用多个解压目录**：如果另解压到新目录启动，新目录会初始化全新的独立数据库——既不会继承旧目录的 Kiro 登录，也会使客户端配置里保存的旧网关密钥失效并报 `HTTP 401 Unauthorized: invalid API key`（若确实迁移了新目录，需重新点击一次客户端保存配置并彻底重启客户端）。
- **严禁外发运行目录**：切勿将运行过的目录（含 `data/`、`backups/`、`logs/`）上传、共享或打包分发。

### 2. Claude Code 操作注意事项

- **自动配置与模型菜单**：在控制台「**4.1 Claude 设置**」选择模型并保存后，会自动更新 `~/.claude/settings.json`（或 `CLAUDE_CONFIG_DIR`），无需填写 Anthropic 官方 API Key；首次修改前自动备份至 `settings.json.keirouter.bak`。所选模型会设为默认（`ANTHROPIC_MODEL`），并自动去重追加到 `modelPicker.options`（保留您原有的其他自定义项与权限配置）。
- **版本与会话生效**：在 Claude Code 内通过 `/model` 菜单切换自定义模型需 **Claude Code ≥ 2.1.242**。修改并保存网关配置后，请退出并重开已有的 `claude` 终端会话。
- **推理强度（`/effort`）**：可在对话中使用 `/effort` 指定档位（如 `low`、`medium`、`high`、`xhigh`、`max`，以所选 Kiro 模型实际支持为准）；未显式指定时网关不会强加默认值，直接沿用 Kiro 上游模型默认设置。

### 3. Codex CLI 与 Codex Desktop 操作注意事项

- **代理接管与备份**：在控制台「**4.2 Codex 设置**」确认保存后，网关会将 `~/.codex/config.toml`（或 `CODEX_HOME`）中的自定义代理统一替换为 Kiro Gateway，原引用其他自定义代理的 `profile` 会自动继承新代理；您的 ChatGPT 登录文件（`auth.json`）与其他无关配置保持不变，替换前完整备份至 `config.toml.before-kiro-takeover.bak`。
- **动态累加模型目录（`kiro-gateway-models.json`）**：每次在「**4.2 Codex 设置**」选择 Kiro 模型并保存时，网关会自动将该模型的展示名称及支持的推理档位**去重追加**到 `~/.codex/kiro-gateway-models.json`，并在 `config.toml` 中关联 `model_catalog_json`。若希望在 Codex 模型菜单中同时保留多个 Kiro 模型，只需在「4.2 Codex 设置」依次选中各个模型并保存即可。
- **配置变更后必须彻底退出后台进程（关键）**：
  - **Codex Desktop（Windows / macOS）**：在 Windows 上点击窗口右上角 `×` **仅会最小化到右下角系统托盘，不会退出进程**。未退出的旧进程（`ChatGPT.exe` 及其子进程 `codex.exe`）不仅会继续使用内存里的旧密钥和旧模型列表，还可能把旧的 `model_reasoning_effort` 重新覆写回 `config.toml`。每次在网关更新密钥或新增模型后，请务必在**系统托盘图标右键彻底退出**（或结束 `ChatGPT.exe` / `codex.exe` 进程）后再重新打开，并点击 **New thread（新建对话）** 以加载新模型的推理档位。
  - **Codex CLI**：请退出旧终端中的 `codex` 进程后重新运行；若本机驻留了旧的后台守护进程（`~/.codex/packages/app-server-daemon/.../codex.exe`）导致仍报 `401`，结束该残留进程后再启动即可。
- **推理档位（Reasoning Effort）与报错排查**：
  - Codex Desktop 在选中模型后可直接从输入框下方菜单切换档位；Codex CLI 可通过 `-m "kiro/claude-opus-5.5" -c model_reasoning_effort=high` 指定单次会话档位。
  - 请仅使用目标模型实际支持的档位（例如 `kiro/claude-opus-5.5` 支持 `low`、`medium`、`high`、`xhigh`、`max`，**不支持 `none`**）。若 `config.toml` 或旧对话线程残留了模型不支持的档位（如 `model_reasoning_effort = "none"`），请求会被校验拦截并在客户端显示 `upstream provider rejected the request`（可在网关控制台日志查看具体原因，将档位切换为 `medium` / `high` 等有效值并新建对话即可恢复）。
- **联网搜索说明**：网关采用纯 HTTPS 直连 Kiro，不依赖 Kiro IDE 或 Kiro CLI。客户端默认携带可选 `WebSearch` / `web_search` 工具声明不会影响正常对话与本地工具调用；显式强制要求执行托管联网搜索的请求会返回 HTTP 400。

## 在线更新

支持在线更新的启动器从本仓库的最新正式 Release 读取签名清单。用户确认后，网关只下载当前平台的升级包，验证 Ed25519 签名和 SHA-256，再备份数据并重启。更新源和发布者公钥可以在“检查更新 → 高级选项”配置。旧启动器的一次性迁移见[绿色版使用说明](./deploy/PORTABLE.md)。

发布者应使用 `vX.Y.Z` 标签，并把 `manifest.json` 和其中列出的各平台升级 ZIP **分别**作为 Release 附件上传；首次安装包另行上传。签名私钥不进入代码库、安装包或 Release。

## 从源码构建

需要 Go 1.26+ 和 Node.js 24。在仓库根目录执行：

```powershell
npm.cmd --prefix frontend ci
npm.cmd --prefix frontend run build
go build -trimpath -o kiro-gateway.exe ./backend/cmd/keirouter
.\kiro-gateway.exe portable --no-browser
```

启动时需要可执行文件旁的 `frontend/dist`。开发构建用于本地验证，不能代替带正式版本号和签名的发布包。

## 许可证

本项目基于 [KeiRouter](https://github.com/mydisha/keirouter) 开发，保留上游版权声明，采用 [MIT 许可证](./LICENSE)。随包第三方依赖按各自许可证分发。
