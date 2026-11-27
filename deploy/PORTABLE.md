# Kiro Gateway

将完整安装包解压到当前用户可写的本地目录。不要只拷贝可执行文件。

- Windows：双击 `kiro-gateway\kiro-gateway.exe`。
- Linux / macOS：进入 `kiro-gateway` 目录，运行 `./kiro-gateway`。如缺少执行权限，先运行 `chmod u+x ./kiro-gateway`。
- 浏览器未自动打开时，访问 `http://127.0.0.1:20180/`。如需指定端口，使用 `portable --port 20311` 启动参数。

在控制台依次进行初始化、企业认证和客户端配置。企业认证使用官方组织登录；不要把含有本地账号、密钥、日志或备份的运行目录分发给其他人。

在“4.2 Codex 设置”确认保存后，Kiro Gateway 成为默认且唯一的自定义模型代理，并将新选中的 Kiro 模型（含推理档位配置）去重补充到 `kiro-gateway-models.json`。引用被移除代理的 profile 会继承新的代理和默认模型；其他设置和登录文件不变。替换前的完整配置保存在原文件旁的 `config.toml.before-kiro-takeover.bak`，请私下保管。完全退出并重新打开 Codex Desktop 以加载配置与模型目录。

从 0.4.13 起，网关发送生成请求时使用自己的客户端标识 `kiro-gateway`。如果旧版本能列出模型，但请求详情提示 “This version of Kiro is no longer supported”，请升级网关并发送一条简短消息验证生成；无需为此更改模型别名或重新登录。

**在线更新**：打开“检查更新”，网关默认检查 `https://github.com/frank-mozi/kiro-gateway` 的最新正式 Release。确认安装后下载当前平台的升级包，校验签名与 SHA-256 后才会停止旧进程。无需 GitHub 登录或手动下载文件。可以在“高级选项”保存其他公开 GitHub 仓库和通过可信渠道核实的发布者公钥。网络或验证失败不会自动改用其他发布源。

**联网搜索（0.4.14）**：Kiro 模型请求直连 HTTP，不启动或连接 Kiro IDE、CLI 或 ACP。网关目前不能通过此接口验证企业 Web Tools 权限，不执行托管搜索，也不会改用第三方搜索。仅声明可选搜索工具不影响普通对话；API 显式强制托管搜索时返回 HTTP 400，不会把模型自身知识当作搜索结果。客户端自定义的同名函数不是托管搜索。

`0.4.15-dev.1` 是本地协议预览，不是联网搜索发布版。回环 HTTPS 测试使用固定搜索结果验证 Claude Messages 和 Codex Responses 的普通及流式返回。预览不含线上授权或搜索适配器，不会因为本地测试通过而启用企业搜索。

在线更新由启动器负责。0.4.11 及更早的启动器需要一次手动迁移：完全退出网关并备份原安装目录，将新版完整包中 `kiro-gateway` 目录的**内容**放入原安装根目录，保留运行数据和备份。如果存在 `current.json`，退出时先将它备份并移出根目录，让新启动器加载随包提供的界面，而不是之前选中的旧版本。从新启动器启动；不要同时运行两个启动器。此后的在线升级会保留账号和设置。

许可证见 `LICENSE` 和 `dependency-notices`。
