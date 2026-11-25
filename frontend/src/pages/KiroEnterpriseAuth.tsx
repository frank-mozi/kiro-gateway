import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type KiroEnterpriseStatus } from "../lib/api";

const storageKey = "kiro-enterprise-session";
const terminal = new Set(["complete", "failed", "cancelled", "expired"]);
const phaseLabels: Record<KiroEnterpriseStatus["phase"], string> = {
  waiting_for_kiro: "等待在 Kiro 页面选择 Your Organization",
  waiting_for_company: "已由 Kiro 跳转到公司认证页面，等待完成登录",
  processing: "正在处理认证并查询 Kiro Profile…",
  select_profile: "认证已返回，请选择本次使用的 Kiro Profile",
  saving: "正在校验 Kiro 模型接口并保存账号…",
  complete: "Kiro 账号已接入网关",
  failed: "认证未完成",
  cancelled: "认证已取消",
  expired: "认证已过期，请重新开始",
};

export function KiroEnterpriseAuth({ busy, perform, onSaved }: {
  busy: boolean;
  perform: (label: string, task: () => Promise<void>) => Promise<boolean>;
  onSaved: () => void;
}) {
  const qc = useQueryClient();
  const [id, setID] = useState(() => sessionStorage.getItem(storageKey) ?? "");
  const [region, setRegion] = useState("us-east-1");
  const [profile, setProfile] = useState("");
  const [popupBlocked, setPopupBlocked] = useState(false);
  const notified = useRef(false);
  const query = useQuery({
    queryKey: ["kiro-enterprise", id], queryFn: () => api.kiroEnterpriseStatus(id),
    enabled: !!id, retry: false,
    refetchInterval: (q) => terminal.has(q.state.data?.phase ?? "") ? false : 1500,
  });
  const status = query.data;
  const active = !!id && !terminal.has(status?.phase ?? "");

  useEffect(() => {
    if (status?.phase === "complete" && !notified.current) {
      notified.current = true;
      sessionStorage.removeItem(storageKey);
      onSaved();
    }
  }, [status?.phase, onSaved]);

  function start() {
    const popup = window.open("about:blank", "_blank");
    if (popup) popup.opener = null;
    setPopupBlocked(!popup);
    void perform("启动 Kiro 企业登录", async () => {
      try {
        const next = await api.kiroEnterpriseStart(region.trim());
        sessionStorage.setItem(storageKey, next.id);
        qc.setQueryData(["kiro-enterprise", next.id], next);
        setID(next.id);
        setProfile("");
        notified.current = false;
        if (popup && next.url) popup.location.href = next.url;
      } catch (error) {
        popup?.close();
        throw error;
      }
    });
  }

  return <div className="manager-form">
    <p>从 <strong>Kiro 官方登录页面</strong>开始，选择 <strong>Your Organization</strong>，
      输入企业邮箱或 Kiro Start URL，再由 Kiro 跳转到公司的认证页面。</p>
    <p>不需要粘贴 Token / JSON，也不需要自行注册认证应用或填写 Tenant ID、Client ID。
      成功后凭据自动保存在本机并用于续期；企业策略要求时仍需重新登录。</p>
    <p className="manager-warning">请在网关所在机器的浏览器完成登录。Ubuntu 部署后，请从 WorkSpaces 桌面打开此控制台；
      不要在 Windows 浏览器登录远程 Ubuntu 网关。当前实现仍需实际企业账号验收。</p>
    {status && <p role="status">{phaseLabels[status.phase]}</p>}
    {(status?.error || query.error) && <div className="manager-error" role="alert">{status?.error || query.error?.message}</div>}
    {status?.phase === "waiting_for_kiro" && status.url && <p>
      {popupBlocked ? "浏览器未自动打开新窗口，请点击：" : "如果登录窗口没有打开："}
      <a href={status.url} target="_blank" rel="noopener noreferrer">打开 Kiro 官方登录页 ↗</a>
    </p>}
    {status?.phase === "select_profile" && <>
      <label>Kiro Profile<select value={profile} onChange={(e) => setProfile(e.target.value)}>
        <option value="">请选择账号下的 Profile</option>
        {status.profiles?.map((arn) => <option key={arn} value={arn}>{arn}</option>)}
      </select></label>
      <button className="manager-primary" disabled={busy || !profile} onClick={() => void perform("接入 Kiro 账号", async () => {
        const next = await api.kiroEnterpriseProfile(id, profile);
        qc.setQueryData(["kiro-enterprise", id], next);
      })}>接入所选 Profile</button>
    </>}
    {!active && <details>
      <summary>高级：Kiro Profile 所在 AWS 区域</summary>
      <label>区域<input value={region} onChange={(e) => setRegion(e.target.value)} placeholder="us-east-1" /></label>
      <p>默认 us-east-1。如果企业 Profile 位于其他区域（例如 eu-central-1），请使用管理员提供的区域。</p>
    </details>}
    <div className="manager-update-actions">
      <button className="manager-primary" disabled={busy || status?.phase === "saving"} onClick={start}>
        {active ? "重新开始 Kiro 登录" : "打开 Kiro · 企业登录"}
      </button>
      {active && <button className="manager-primary" disabled={busy || status?.phase === "saving"} onClick={() => void perform("取消认证", async () => {
        await api.kiroEnterpriseCancel(id);
        sessionStorage.removeItem(storageKey);
        setID("");
      })}>取消本次认证</button>}
    </div>
    <p>关闭此面板不会取消登录；可重新打开查看进度。会话有效期 10 分钟，重新开始会取消上一次登录。</p>
    <Link to="/providers/kiro">高级账号管理（可选凭据导入）↗</Link>
  </div>;
}
