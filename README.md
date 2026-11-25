# Kiro Gateway

Kiro Gateway 把 Kiro 企业账号接入本机网关，供 Claude Code CLI 和 Codex CLI 使用。它通过 Kiro 官方组织登录流程认证，默认只监听 `127.0.0.1:20180`，无需 sudo 或 systemd。本项目不是 Kiro 官方产品。

## 快速开始

1. 从 [GitHub Releases](https://github.com/frank-mozi/kiro-gateway/releases) 选择适合平台的 **`kiro-gateway_portable-…`** 完整包：Windows x64 用 ZIP，Linux x64 和 Intel macOS 用 TAR.GZ。不要把供自动更新使用的平台 ZIP 当作首次安装包。
2. 将整个包解压到固定的、仅当前用户可写的目录。Windows 双击 `kiro-gateway.exe`；Linux/macOS 在解压目录运行 `./kiro-gateway`。如果浏览器没有打开，手动访问 <http://127.0.0.1:20180/>。
3. 在控制台运行“初始化 / 检查”，再点“认证”。在 Kiro 页面选 **Your Organization**，填写工作邮箱并完成组织身份认证。
4. 启用模型 API，在“Claude 设置”或“Codex 设置”中选择模型并保存。Claude Code 可用 `/effort` 指定推理强度；未显式指定时沿用 Kiro 默认值。实际可调用的模型取决于账号权限。

**登录状态保存在安装目录的 `data` 中。** 从同一目录重启通常不需要重新登录；另解压到新目录不会自动继承账号。不要公开或分发已运行过的目录，其中可能有数据库、密钥、日志和备份。

**Kiro 企业认证不需要 Google 或 iFlow 凭据。** Gemini CLI、Antigravity 和 iFlow 的独立 OAuth 登录入口默认关闭。如需使用某个入口，请在网关进程的环境中配置自己的 OAuth 应用：分别以 `KIRO_GATEWAY_GEMINI_CLI`、`KIRO_GATEWAY_ANTIGRAVITY` 或 `KIRO_GATEWAY_IFLOW` 为前缀，同时设置 `_CLIENT_ID` 和 `_CLIENT_SECRET`，然后重启。不要把值写进源码或分发包。

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
