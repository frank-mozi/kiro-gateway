import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type PortableSource, type PortableState } from "../lib/api";
import { useConfirm } from "../components/ui/confirm-dialog";

type Perform = (label: string, task: () => Promise<void>) => Promise<boolean>;
const activePhases = new Set(["verifying", "extracting", "draining", "stopping", "backing_up", "restarting", "rolling_back"]);
const phaseNames: Record<string, string> = {
  idle: "等待检查", checked: "检查完成", check_failed: "检查失败", verifying: "验证发布签名",
  extracting: "校验并解压安装包", draining: "等待进行中的模型请求结束（最多 60 秒）",
  stopping: "停止旧进程", backing_up: "备份数据和凭据", restarting: "启动并验证新版",
  rolling_back: "正在恢复旧版本及数据", rolled_back: "升级失败，已恢复旧版本",
  completed: "升级完成", failed: "操作失败，请查看错误及运行日志",
};

export function PortableUpdates({ busy, perform }: { busy: boolean; perform: Perform }) {
  const qc = useQueryClient();
  const confirm = useConfirm();
  const checked = useRef(false);
  const status = useQuery({
    queryKey: ["portable-status"], queryFn: api.portableStatus, retry: false, refetchInterval: 2000,
  });
  const state = status.data;
  const updating = !!state && activePhases.has(state.phase);
  const accept = (result: PortableState) => {
    qc.setQueryData(["portable-status"], result);
    void qc.invalidateQueries({ queryKey: ["update-check"] });
    void qc.invalidateQueries({ queryKey: ["gateway-control"] });
  };
  useEffect(() => {
    if (checked.current || !state) return;
    checked.current = true;
    if (state.managed && state.source.repository && ["idle", "checked", "check_failed"].includes(state.phase)) {
      void perform("检查在线更新", async () => {
        try { accept(await api.portableCheck()); }
        finally { void qc.invalidateQueries({ queryKey: ["portable-status"] }); }
      });
    }
  }, [state, perform, qc]);

  if (!state) return <div className="manager-form"><p role={status.isError ? "alert" : "status"}>{status.isError ? status.error.message : "正在读取绿色版状态…"}</p></div>;
  if (!state.managed) return <div className="manager-form">
    <p className="manager-warning">当前不是绿色版管理模式，不能安全地自更新。</p>
    <p>请停止此实例，然后在完整解压目录运行 <code>./kiro-gateway portable</code>（Windows 使用 <code>.\kiro-gateway.exe portable</code>）。无需 sudo 或 systemd。</p>
    <p>绿色版使用安装目录内独立的 data 文件夹；不会自动搬移旧实例的账号或配置。</p>
  </div>;
  if (!state.source.repository) return <div className="manager-form"><p className="manager-warning">当前启动器仅支持旧版本地更新。请退出网关，将新版完整绿色包解压到原安装根目录，保留 data、版本指针与备份，再从新版启动器启动。</p></div>;
  return <div className="manager-form">
    <p>在线检查 GitHub Releases 上的最新稳定版。确认安装后才会下载当前平台的升级包，并验证发布者签名和文件校验值。</p>
    <p>当前版本：<strong>{state.current}</strong> · 在线发布版本：<strong>{state.checked_at ? state.latest : "尚未成功检查"}</strong></p>
    <p role="status">{phaseNames[state.phase] ?? state.phase}{state.checked_at && ` · 最近成功检查 ${new Date(state.checked_at).toLocaleString()}`}</p>
    {status.isError && <p role="alert">{updating ? "正在重启或暂时无法连接，恢复后会继续读取状态。" : `无法读取状态：${status.error.message}`}</p>}
    {state.error && <div className="manager-error" role="alert">{state.error}</div>}
    {state.notes && <pre className="manager-release-notes">{state.notes}</pre>}
    <details><summary>高级选项：更新源与发布者公钥</summary>
      <SourceForm key={`${state.source.repository}:${state.source.public_key}`} source={state.source}
        disabled={busy || updating || status.isError} onSave={(source) => void perform("保存发布源", async () => accept(await api.portableSource(source)))} />
    </details>
    <div className="manager-update-actions">
      <button className="manager-primary" disabled={busy || updating || status.isError}
        onClick={() => void perform("检查在线更新", async () => {
          try { accept(await api.portableCheck()); }
          finally { void qc.invalidateQueries({ queryKey: ["portable-status"] }); }
        })}>检查在线更新</button>
      <button className="manager-primary" disabled={busy || updating || status.isError || !state.update_available}
        onClick={() => void (async () => {
          if (!await confirm({
            title: `升级至 ${state.latest}？`,
            description: "将验证签名、等待模型请求结束、停止网关并备份数据。管理页面会短暂断开；新版启动验证失败时恢复旧版和升级前数据。请保持绿色版启动窗口运行，不要同时修改数据文件。",
            confirmLabel: "确认升级", cancelLabel: "取消",
          })) return;
          await perform("提交升级", async () => accept(await api.portableInstall(state.latest)));
        })()}>安装更新</button>
      {state.phase === "completed" && <button className="manager-primary" onClick={() => window.location.reload()}>刷新加载新版界面</button>}
    </div>
    <p>启动器版本：{state.launcher}。升级保留原始启动器；今后若需要升级启动器本身，需退出后手动替换完整绿色包。</p>
  </div>;
}

function SourceForm({ source, disabled, onSave }: {
  source: PortableSource; disabled: boolean; onSave: (source: PortableSource) => void;
}) {
  const [repository, setRepository] = useState(source.repository ?? "");
  const [publicKey, setPublicKey] = useState(source.public_key);
  return <form className="manager-form" onSubmit={(event) => {
    event.preventDefault(); onSave({ repository: repository.trim(), public_key: publicKey.trim() });
  }}>
    <label>GitHub 更新源<input type="url" required disabled={disabled} value={repository} onChange={(e) => setRepository(e.target.value)}
      placeholder="https://github.com/owner/repo" /></label>
    <label>发布者公钥<input required disabled={disabled} value={publicKey} onChange={(e) => setPublicKey(e.target.value)}
      autoComplete="off" spellCheck={false} placeholder="通过可信渠道取得的 Ed25519 公钥；不是私钥" /></label>
    <button className="manager-primary" disabled={disabled}>保存发布源</button>
  </form>;
}
