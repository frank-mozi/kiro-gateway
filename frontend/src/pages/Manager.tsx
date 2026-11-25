import { useEffect, useMemo, useRef, useState, type FormEvent, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Dialog, Select } from "radix-ui";
import { Tooltip, TooltipTrigger, TooltipContent } from "../components/ui/tooltip";
import { Activity, Braces, CheckCheck, CircleHelp, Cpu, Download, KeyRound, Layers3, Pause, Play, Settings2, ShieldCheck, Terminal, Trash2, X } from "lucide-react";
import { api, type EndpointSettings } from "../lib/api";
import { useConfirm } from "../components/ui/confirm-dialog";
import { PortableUpdates } from "./PortableUpdates";
import { KiroEnterpriseAuth } from "./KiroEnterpriseAuth";
import "./manager.css";

type PanelName = "auth" | "claude" | "codex" | "proxy" | "usage" | "update" | null;
type Perform = (label: string, task: () => Promise<void>) => Promise<boolean>;
interface Line { id: string; ts: number; time: string; level: string; msg: string }
const kiroModelQuery = {
  queryKey: ["providerModels", "kiro", "llm"],
  queryFn: () => api.providerModels("kiro", "llm"),
  staleTime: 60_000,
  retry: false,
};

export function ManagerPage() {
  const qc = useQueryClient();
  const confirm = useConfirm();
  const [panel, setPanel] = useState<PanelName>(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [live, setLive] = useState(true);
  const [follow, setFollow] = useState(true);
  const [localLines, setLocalLines] = useState<Line[]>([]);
  const sequence = useRef(0);
  const logView = useRef<HTMLDivElement>(null);
  const control = useQuery({
    queryKey: ["gateway-control"], queryFn: api.gatewayControl, refetchInterval: 5000, retry: false,
  });
  const accounts = useQuery({
    queryKey: ["manager-accounts"], queryFn: api.listAccounts, refetchInterval: 30000,
  });
  const logs = useQuery({
    queryKey: ["manager-logs"], queryFn: api.consoleLog, enabled: live,
    refetchInterval: live ? 2000 : false, retry: false,
  });
  const kiroAccounts = accounts.data?.accounts.filter((a) => a.provider === "kiro") ?? [];
  const reconnectAccounts = kiroAccounts.filter((a) => a.needs_reconnect);
  const lines = useMemo(() => {
    const remote: Line[] = (logs.data?.logs ?? []).map((line) => ({
      id: `${logs.data?.epoch}:${line.seq}`, ts: line.ts ?? 0,
      time: line.time, level: line.level, msg: line.msg,
    }));
    return [...remote, ...localLines].sort((a, b) => a.ts - b.ts).slice(-1000);
  }, [logs.data, localLines]);

  useEffect(() => {
    document.title = "Kiro Gateway · 控制台";
    document.body.classList.add("gateway-console");
    return () => document.body.classList.remove("gateway-console");
  }, []);
  useEffect(() => {
    if (follow && logView.current) logView.current.scrollTop = logView.current.scrollHeight;
  }, [lines, follow]);

  function append(msg: string, level = "INFO") {
    const now = new Date();
    const line: Line = {
      id: `local:${++sequence.current}`, ts: now.getTime(),
      time: now.toLocaleTimeString("zh-CN", { hour12: false }), level, msg,
    };
    setLocalLines((previous) => [...previous, line].slice(-200));
  }
  const perform: Perform = async (label, task) => {
    setBusy(label);
    setError("");
    try {
      await task();
      return true;
    } catch (reason) {
      const message = reason instanceof Error ? reason.message : String(reason);
      setError(`${label}失败：${message}`);
      append(`${label}失败：${message}`, "ERROR");
      return false;
    } finally {
      setBusy("");
    }
  };
  const open = (next: PanelName) => { setError(""); setPanel(next); };
  const close = () => { if (!busy) { setPanel(null); setError(""); } };
  const initialized = async () => {
    const [state, accountList, tools, keys] = await Promise.all([
      api.gatewayControl(), api.listAccounts(), api.cliTools(), api.listKeys(),
    ]);
    qc.setQueryData(["gateway-control"], state);
    qc.setQueryData(["manager-accounts"], accountList);
    const kiro = accountList.accounts.filter((a) => a.provider === "kiro");
    append(`初始化检查完成\n程序：${state.version} · 监听 ${state.listen_addr}\nKiro 账号：${kiro.length} 个 · 有效网关密钥：${keys.keys.filter((k) => !k.disabled).length} 个\n${tools.tools.filter((t) => t.id === "claude" || t.id === "codex").map((t) => `${t.name}：${t.error ? `检测失败 (${t.error})` : t.installed ? "已检测到" : "未检测到"}；${t.configured ? "已有配置" : "未配置"}`).join("\n")}\n程序已部署，本操作不安装软件、不修改账号或客户端配置。`);
  };
  const setEnabled = (enabled: boolean) => perform(enabled ? "启动" : "停止", async () => {
    const next = await api.setGatewayControl(enabled);
    qc.setQueryData(["gateway-control"], next);
    append(enabled ? "模型 API 已启用；客户端可以发送新请求。" : "模型 API 已暂停；新模型请求返回 503，管理页面、模型查询和已开始的请求保持可用。", enabled ? "INFO" : "WARN");
  });
  const loadModels = () => perform("查看模型", async () => {
    const result = await qc.fetchQuery({ ...kiroModelQuery, staleTime: 0 });
    append(`Kiro 网关模型目录 · ${result.models.length} 项\n包含上游动态结果、静态配置和派生别名；不代表账号实时授权清单。\n${result.models.map((m) => `kiro/${m.id}  ${m.name}`).join("\n") || "当前目录为空。"}`);
  });
  const clearLogs = async () => {
    if (!await confirm({
      title: "清空运行日志？", description: "清空服务端日志缓冲区及本页输出，不删除 Usage、审计记录或认证数据。",
      confirmLabel: "清空日志", cancelLabel: "取消", tone: "danger",
    })) return;
    await perform("清空日志", async () => {
      await qc.cancelQueries({ queryKey: ["manager-logs"] });
      await api.clearConsoleLog();
      qc.setQueryData(["manager-logs"], { epoch: "", logs: [] });
      setLocalLines([]);
    });
  };
  const titles = { auth: "Kiro 企业认证", claude: "Claude Code 设置", codex: "Codex CLI 设置", proxy: "代理高级配置", usage: "Usage · 用量", update: "检查更新" };
  const stateUnknown = !control.data || control.isError;
  const enabled = !stateUnknown && control.data?.enabled;
  const errors = [error, control.error?.message, accounts.error?.message, live ? logs.error?.message : undefined].filter(Boolean);

  return (
    <main className="manager" lang="zh-CN">
      <div className="manager-shell">
        <header className="manager-header">
          <div className="manager-brand">
            <div className="manager-mark"><Layers3 aria-hidden="true" /></div>
            <div><p className="manager-eyebrow">LOCAL AI GATEWAY</p><h1>Kiro Gateway <span>控制台</span></h1></div>
          </div>
          <div className="manager-header-tools">
            <button className="manager-advanced" disabled={!!busy} onClick={() => open("update")}><Download aria-hidden="true" size={15} />检查更新</button>
          </div>
        </header>
        <section className="manager-status" aria-label="网关状态">
          <span className={`manager-dot ${stateUnknown ? "unknown" : enabled ? "running" : "paused"}`} />
          <div role="status">
            <p className="manager-eyebrow">GATEWAY STATUS</p>
            <strong>{stateUnknown ? "正在连接或无法连接管理服务" : enabled ? "模型 API 已启用" : "模型 API 已暂停"}</strong>
            <span>{control.data ? `管理端口 ${control.data.listen_addr} · ${control.data.version}` : "等待状态响应"}</span>
          </div>
          <div className="manager-account"><ShieldCheck aria-hidden="true" size={20} /><div><p className="manager-eyebrow">KIRO CONNECTION</p>{accounts.isError ? "账号读取失败" : accounts.isPending ? "检查账号中…" : `Kiro 账号 ${kiroAccounts.length} 个 · 待重连 ${reconnectAccounts.length} 个`}</div></div>
        </section>
        <div className="manager-section-heading"><h2>工作区</h2><span>连接 Kiro，让本地客户端就绪</span></div>
        <nav className="manager-actions" aria-label="网关操作">
          <button disabled={!!busy} onClick={() => void perform("初始化检查", initialized)}><CheckCheck aria-hidden="true" /><span>1. 初始化 / 检查</span><small>检查环境与客户端</small></button>
          <button disabled={!!busy} onClick={() => open("auth")}><ShieldCheck aria-hidden="true" /><span>2. 认证</span><small>Kiro 企业登录</small></button>
          <button className="green" disabled={!!busy || stateUnknown || enabled} onClick={() => void setEnabled(true)}><Play aria-hidden="true" /><span>3. 启动</span><small>启用模型 API</small></button>
          <button className="red" disabled={!!busy || stateUnknown || !enabled} onClick={() => void setEnabled(false)}><Pause aria-hidden="true" /><span>停止</span><small>暂停新的模型请求</small></button>
          <button disabled={!!busy || stateUnknown} onClick={() => open("claude")}><Terminal aria-hidden="true" /><span>4.1 Claude 设置</span><small>自动配置本地连接</small></button>
          <button disabled={!!busy || stateUnknown} onClick={() => open("codex")}><Braces aria-hidden="true" /><span>4.2 Codex 设置</span><small>自动配置本地连接</small></button>
        </nav>
        <nav className="manager-tools" aria-label="网关工具">
          <button disabled={!!busy} onClick={() => open("proxy")}><Settings2 aria-hidden="true" />代理高级配置</button>
          <button disabled={!!busy} onClick={() => void loadModels()}><Cpu aria-hidden="true" />查看模型</button>
          <button disabled={!!busy} onClick={() => open("usage")}><Activity aria-hidden="true" />Usage · 用量</button>
        </nav>
        <div className="manager-hint" role="status"><CircleHelp aria-hidden="true" size={14} />{busy ? `${busy}中…` : "“停止”仅暂停新模型请求；管理控制台与正在进行的请求保持可用。"}</div>
        {errors.length > 0 && <div className="manager-error" role="alert">{errors.join(" · ")}</div>}
        <section className="manager-output" aria-label="日志输出">
          <div className="manager-log-heading">
            <h2><Terminal aria-hidden="true" size={16} />日志输出</h2><span className="manager-log-cadence">{live ? "LIVE · 2s" : "PAUSED"} · 最多 1000 行</span>
            <div className="manager-log-tools">
              <label><input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> 自动滚动</label>
              <button aria-pressed={live} onClick={() => setLive(!live)}>{live ? <Pause aria-hidden="true" /> : <Play aria-hidden="true" />}{live ? "暂停实时日志" : "开启实时日志"}</button>
              <button disabled={!!busy} onClick={() => void clearLogs()}><Trash2 aria-hidden="true" />清空日志</button>
            </div>
          </div>
          <div className="manager-log" ref={logView} tabIndex={0} aria-label="运行日志">
            {!lines.length && <div className="manager-empty"><Terminal aria-hidden="true" size={28} /><strong>准备就绪，等待活动</strong><p>点击“初始化 / 检查”检查网关，或查看模型目录。</p></div>}
            {lines.map((line) => <div key={line.id} className={`manager-line ${line.level.toLowerCase()}`}><time>[{line.time}]</time><pre>{line.msg}</pre></div>)}
          </div>
        </section>
        <footer className="manager-footer"><span>本地部署 · 凭据保存在网关所在机器</span><span>Kiro 官方企业登录 · 本机浏览器回调</span></footer>
      </div>
      {panel && <ManagerDialog title={titles[panel]} onClose={close} busy={!!busy} error={error}>
        {panel === "auth" && <KiroEnterpriseAuth perform={perform} busy={!!busy} onSaved={() => {
          void qc.invalidateQueries({ queryKey: ["manager-accounts"] });
          void qc.invalidateQueries({ queryKey: ["accounts"] });
          append("Kiro 企业账号已接入网关；凭据已保存，后续由网关自动续期。企业策略要求时仍需重新认证。"); setPanel(null);
        }} />}
        {(panel === "claude" || panel === "codex") && <ClientForm key={panel} toolId={panel} baseUrl={control.data?.base_url ?? ""} busy={!!busy} perform={perform} onSaved={() => {
          void qc.invalidateQueries({ queryKey: ["cli-tools"] });
          void qc.invalidateQueries({ queryKey: ["manager-cli"] });
          append(`${panel === "claude" ? "Claude Code" : "Codex CLI"} 配置已写入服务端当前用户目录。请重启客户端。`); setPanel(null);
        }} />}
        {panel === "proxy" && <ProxyForm busy={!!busy} perform={perform} onSaved={() => { append("出站代理配置已保存。"); setPanel(null); }} />}
        {panel === "usage" && <UsagePanel />}
        {panel === "update" && <PortableUpdates busy={!!busy} perform={perform} />}
      </ManagerDialog>}
    </main>
  );
}

function ManagerDialog({ title, children, onClose, busy, error }: {
  title: string; children: ReactNode; onClose: () => void; busy: boolean; error: string;
}) {
  return <Dialog.Root open onOpenChange={(open) => { if (!open && !busy) onClose(); }}>
    <Dialog.Portal>
      <Dialog.Overlay className="manager-overlay" />
      <Dialog.Content className="manager-dialog" lang="zh-CN" aria-describedby={undefined}
        onEscapeKeyDown={(e) => { if (busy) e.preventDefault(); }} onPointerDownOutside={(e) => e.preventDefault()}>
        <header><div><p className="manager-eyebrow">GATEWAY CONSOLE</p><Dialog.Title>{title}</Dialog.Title></div><Dialog.Close disabled={busy} aria-label="关闭"><X aria-hidden="true" size={20} /></Dialog.Close></header>
        {error && <div className="manager-error" role="alert">{error}</div>}
        {children}
      </Dialog.Content>
    </Dialog.Portal>
  </Dialog.Root>;
}

function creditRateLabel(rate?: number): string {
  return rate !== undefined && Number.isFinite(rate) && rate >= 0 ? `积分倍率 ${rate}×` : "积分倍率未知";
}

function ClientForm({ toolId, baseUrl, busy, perform, onSaved }: {
  toolId: "claude" | "codex"; baseUrl: string; busy: boolean; perform: Perform; onSaved: () => void;
}) {
  const query = useQuery({ queryKey: ["manager-cli", toolId], queryFn: () => api.cliTool(toolId), staleTime: 0 });
  const catalog = useQuery(kiroModelQuery);
  const confirm = useConfirm();
  const [url, setURL] = useState(baseUrl);
  const [model, setModel] = useState("");
  const [manualModel, setManualModel] = useState(false);
  const [keyMode, setKeyMode] = useState("auto");
  const [key, setKey] = useState("");
  const seeded = useRef(false);
  const createdKey = useRef("");
  const [hoverModel, setHoverModel] = useState("");
  const keyboardNavigation = useRef(false);
  const modelOptions = useMemo(() => {
    const groups = new Map<string, { id: string; name: string; rates: Map<string, number | undefined> }>();
    const entries = catalog.data?.models ?? [];
    const ids = new Set(entries.map(e => e.id.replace(/^kiro\//, "")));
    for (const entry of entries) {
      const local = entry.id.replace(/^kiro\//, "");
      const stripped = local.replace(/-(thinking-agentic|thinking|agentic)$/, "");
      const base = ids.has(stripped) ? stripped : local;
      const id = `kiro/${base}`;
      const group = groups.get(id) ?? { id, name: base, rates: new Map<string, number | undefined>() };
      if (local === base) group.name = (entry.name || base).replace(/ \([\d.e+-]+x credit\)/, "");
      group.rates.set(local === base ? "normal" : local.slice(base.length + 1), entry.rate_multiplier);
      groups.set(id, group);
    }
    return groups;
  }, [catalog.data]);
  const savedBase = model.replace(/-(thinking-agentic|thinking|agentic)$/, "");
  const selected = modelOptions.has(model) ? model : modelOptions.has(savedBase) ? savedBase : model;
  useEffect(() => {
    if (!query.data || query.isFetching || seeded.current) return;
    seeded.current = true;
    const stored = query.data.settings;
    if (stored?.endpoint_match) { setURL(stored.base_url); setModel(stored.model ?? ""); }
  }, [query.data, query.isFetching]);
  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (!await confirm({
      title: "写入客户端配置？",
      description: `将修改网关所在机器当前用户的 ${toolId === "claude" ? "Claude Code" : "Codex CLI"} 配置，不修改浏览器所在的其他机器。保留其他配置项，但格式或注释可能变化；首次修改前会备份已有文件。`,
      confirmLabel: "确认写入", cancelLabel: "取消",
    })) return;
    await perform("配置客户端", async () => {
      let apiKey: string | undefined;
      if (keyMode === "paste") apiKey = key.trim();
      const needsKey = keyMode === "new" || (keyMode === "auto" && (await api.cliTool(toolId)).settings?.key_status !== "valid");
      if (needsKey) {
        if (!createdKey.current) createdKey.current = (await api.createKey(`${toolId} (compact console)`)).key;
        apiKey = createdKey.current;
      }
      await api.cliToolConfigure(toolId, { base_url: url.trim(), model: model.trim(), api_key: apiKey });
      createdKey.current = ""; setKey(""); onSaved();
    });
  };
  if (query.isPending || (!seeded.current && query.isFetching)) return <p>正在读取客户端配置…</p>;
  if (query.isError) return <p role="alert" className="manager-error">{query.error.message}</p>;
  const tool = query.data;
  return <form className="manager-form" onSubmit={(e) => void save(e)}>
    <p>{tool.error ? `检测问题：${tool.error}` : tool.installed ? "已检测到客户端" : "尚未检测到客户端，可先生成配置；本操作不安装 CLI。"}<br />配置位置：<code>{tool.config_files?.join(", ") || tool.config_path}</code></p>
    <label>网关地址<input required type="url" value={url} onChange={(e) => setURL(e.target.value)} /></label>
    <label>模型 ID</label>
    <Select.Root value={manualModel ? "__manual__" : selected} onOpenChange={() => { keyboardNavigation.current = false; setHoverModel(""); }} onValueChange={(value) => {
      const manual = value === "__manual__";
      setManualModel(manual);
      if (!manual) setModel(value);
    }}>
      <Select.Trigger type="button" className="manager-advanced" aria-label="模型 ID">
        <Select.Value placeholder={catalog.isPending ? "正在加载模型…" : "请选择模型"} /><Select.Icon> ▾</Select.Icon>
      </Select.Trigger>
      <Select.Content position="popper" className="manager-model-menu" sideOffset={4}
        onKeyDownCapture={() => { keyboardNavigation.current = true; }}
        onPointerMoveCapture={() => { keyboardNavigation.current = false; }}>
        <Select.Viewport className="manager-model-list" onScroll={() => setHoverModel("")}>
          {model && !modelOptions.has(selected) && <Select.Item value={model}><Select.ItemText>当前配置：{model}（目录外）</Select.ItemText></Select.Item>}
          {Array.from(modelOptions.values(), entry => <Tooltip key={entry.id} open={hoverModel === entry.id} disableHoverableContent>
            <TooltipTrigger asChild>
              <Select.Item value={entry.id} onPointerMove={e => { if (e.pointerType === "mouse") setHoverModel(entry.id); }}
                onPointerLeave={() => setHoverModel("")} onBlur={() => setHoverModel("")}
                onFocus={() => { if (keyboardNavigation.current) setHoverModel(entry.id); }}>
                <Select.ItemText>{entry.name}</Select.ItemText>
              </Select.Item>
            </TooltipTrigger>
            <TooltipContent side="right" className="manager-rate-popup">
              <strong>{entry.name}</strong>
              <div>基础模型：{creditRateLabel(entry.rates.get("normal"))}</div>
              {Array.from(entry.rates).filter(([mode]) => mode !== "normal").map(([mode, rate]) =>
                <div key={mode}>{mode.replace("-", " + ")}：{rate !== undefined && rate === entry.rates.get("normal") ? "沿用基础倍率" : creditRateLabel(rate)}</div>)}
              <small>积分倍率不是每次请求的实际扣费。</small>
            </TooltipContent>
          </Tooltip>)}
          <Select.Item value="__manual__"><Select.ItemText>手动输入模型 ID…</Select.ItemText></Select.Item>
        </Select.Viewport>
      </Select.Content>
    </Select.Root>
    {manualModel && <label>自定义模型 ID<input required value={model} onChange={(e) => setModel(e.target.value)} placeholder="kiro/模型ID" autoComplete="off" spellCheck={false} /></label>}
    {!manualModel && selected !== model && <p>保留已有配置：<code>{model}</code>。新选择使用基础模型 ID。</p>}
    <div className="manager-model-status">
      <p role="status">{catalog.isFetching ? "正在刷新模型目录…" : `网关目录 ${modelOptions.size} 个模型。展开后悬停或用方向键查看各模式倍率。`}</p>
      <button type="button" className="manager-advanced" disabled={catalog.isFetching || busy} onClick={() => void catalog.refetch()}>刷新模型</button>
    </div>
    {catalog.isError && <p className="manager-error" role="alert">模型目录加载失败：{catalog.error.message}。可以重试或手动输入模型 ID。</p>}
    {!catalog.isPending && !catalog.isError && !modelOptions.size && <p>当前目录为空；可手动输入已确认可用的模型 ID。</p>}
    {toolId === "claude" && <p>保存时将所选模型设为默认，并补充到 <code>modelPicker.options</code>；已有条目不重复添加。Claude Code ≥ 2.1.242 可在 <code>/model</code> 菜单选择，组织策略仍可能限制或覆盖此列表。</p>}
    <p className="manager-auto-note"><ShieldCheck aria-hidden="true" size={18} />{keyMode === "auto" ? "本地连接自动配置，无需申请或填写官方 API Key。" : "已启用高级令牌设置；这不是上游服务的官方 API Key。"}</p>
    <details className="manager-advanced-settings">
      <summary><Settings2 aria-hidden="true" size={15} />高级设置</summary>
      <div className="manager-form">
        <label>网关访问令牌<select value={keyMode} onChange={(e) => setKeyMode(e.target.value)}>
          <option value="auto">自动管理（推荐）</option>
          {tool.settings?.key_status === "valid" && <option value="keep">保留客户端已有的有效令牌</option>}
          <option value="new">创建新的访问令牌</option><option value="paste">使用已有网关令牌</option>
        </select></label>
        {keyMode === "paste" && <label>已有网关令牌<input required type="password" autoComplete="off" value={key} onChange={(e) => setKey(e.target.value)} /></label>}
        <p>自动模式会复用有效令牌，没有时自动生成并写入客户端配置。令牌仅用于访问本地网关，不是 Kiro 企业登录凭据。</p>
        <p>配置失败可直接重试，本次生成的令牌会复用。取消后可在令牌管理中撤销未使用的令牌。</p>
        <Link to="/keys"><KeyRound aria-hidden="true" size={14} />管理网关令牌 ↗</Link>
      </div>
    </details>
    <button className="manager-primary" disabled={busy || !model.trim() || !url.trim()}>保存客户端配置</button>
  </form>;
}

function ProxyForm({ busy, perform, onSaved }: { busy: boolean; perform: Perform; onSaved: () => void }) {
  const query = useQuery({ queryKey: ["endpoint-settings"], queryFn: api.endpointSettings, staleTime: 0 });
  if (query.isPending) return <p>正在读取代理配置…</p>;
  if (query.isError) return <p className="manager-error" role="alert">{query.error.message}</p>;
  return <ProxyFields initial={query.data} busy={busy} perform={perform} onSaved={onSaved} />;
}

function ProxyFields({ initial, busy, perform, onSaved }: {
  initial: EndpointSettings; busy: boolean; perform: Perform; onSaved: () => void;
}) {
  const qc = useQueryClient();
  const [enabled, setEnabled] = useState(initial.outbound_proxy_enabled);
  const [url, setURL] = useState(initial.outbound_proxy_url);
  const [noProxy, setNoProxy] = useState(initial.outbound_no_proxy);
  return <form className="manager-form" onSubmit={(e) => {
    e.preventDefault();
    void perform("保存代理", async () => {
      if (enabled) {
        let parsed: URL;
        try { parsed = new URL(url.trim()); } catch { throw new Error("请输入完整的代理 URL。"); }
        if (!["http:", "https:", "socks5:", "socks5h:"].includes(parsed.protocol)) throw new Error("代理协议须为 HTTP、HTTPS 或 SOCKS5。");
      }
      const settings = await api.updateEndpointSettings({ outbound_proxy_enabled: enabled, outbound_proxy_url: url.trim(), outbound_no_proxy: noProxy.trim() });
      qc.setQueryData(["endpoint-settings"], settings); onSaved();
    });
  }}>
    <p>配置网关访问上游的出站代理，不更改本机监听端口。账号专属代理池仍可能覆盖全局设置。</p>
    <label className="manager-check"><input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />启用全局出站代理</label>
    <label>代理 URL<input type="password" autoComplete="off" required={enabled} value={url} onChange={(e) => setURL(e.target.value)} placeholder="http://127.0.0.1:8080 或 socks5://127.0.0.1:1080" /></label>
    <label>不走代理的地址（逗号分隔）<input value={noProxy} onChange={(e) => setNoProxy(e.target.value)} placeholder="localhost,127.0.0.1" /></label>
    <button className="manager-primary" disabled={busy}>保存代理配置</button>
    <Link to="/proxy-pools">高级代理池管理 ↗</Link>
  </form>;
}

function UsagePanel() {
  const [period, setPeriod] = useState("today");
  const query = useQuery({ queryKey: ["manager-usage", period], queryFn: () => api.usage(period), refetchInterval: 10000 });
  return <div className="manager-form">
    <label>统计范围<select value={period} onChange={(e) => setPeriod(e.target.value)}><option value="today">今天</option><option value="7d">最近 7 天</option><option value="30d">最近 30 天</option></select></label>
    {query.isPending ? <p>正在读取用量…</p> : query.isError ? <p role="alert" className="manager-error">{query.error.message}</p> : <dl className="manager-metrics">
      <div><dt>请求数</dt><dd>{query.data.total_requests.toLocaleString()}</dd></div>
      <div><dt>输入 Token</dt><dd>{query.data.prompt_tokens.toLocaleString()}</dd></div>
      <div><dt>输出 Token</dt><dd>{query.data.completion_tokens.toLocaleString()}</dd></div>
      <div><dt>估算费用 USD</dt><dd>${query.data.cost_usd.toFixed(4)}</dd></div>
    </dl>}
    <p>这是网关记录的全部提供商用量，不是 Kiro 官方余额或账单。费用取决于配置的价格表。</p>
    <Link to="/usage">查看详细用量 ↗</Link>
  </div>;
}
