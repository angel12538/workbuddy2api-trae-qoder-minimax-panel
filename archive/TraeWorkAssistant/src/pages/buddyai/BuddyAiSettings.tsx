import { useCallback, useEffect, useRef, useState } from 'react';
import { ListChecks, RefreshCw, Save, Search, SlidersHorizontal } from 'lucide-react';
import { Badge, Spinner } from '../../components/ui';
import PageHeader from '../../components/PageHeader';
import { api } from '../../lib/tauri';
import { useAppStore } from '../../store';
import type { AppLocate } from '../../types';

/**
 * BuddyAI · 环境配置页（整体左右布局对齐国内 Buddy「环境配置」）。
 *
 * 左：通用配置（应用环境——WorkBuddy AI / CodeBuddy AI 国际客户端路径，自动检测预填 +
 * 人工指定，随「保存配置」统一提交；探测走 workbuddy_ai / codebuddy_ai 独立档案）。
 * 右：任务配置（JWT Token 定时续期 / 每日自动打卡 / 积分数据同步，开关 + 每日时刻均可
 * 在本页修改）。应用入口可见性统一收敛到「系统设置 → 应用显示」，本页不再重复；
 * 模型目录开关归「资源调度」页。
 */

/** 任务行表单（开关 + 每日时刻） */
interface TaskForm {
  enabled: boolean;
  hhmm: string;
}

/** 任务配置行：开关 + 每日时刻（随「保存配置」统一生效） */
function TaskRow({
  title,
  desc,
  form,
  onChange,
}: {
  title: string;
  desc: string;
  form: TaskForm;
  onChange: (f: TaskForm) => void;
}) {
  return (
    <div className="rounded-lg border border-slate-100 p-3 dark:border-zinc-800">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <label className="flex cursor-pointer items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={form.enabled}
            onChange={(e) => onChange({ ...form, enabled: e.target.checked })}
          />
          <span className="font-medium">{title}</span>
          <Badge tone={form.enabled ? 'green' : 'amber'}>{form.enabled ? '已开启' : '未开启'}</Badge>
        </label>
        <div className="flex items-center gap-2">
          <span className="text-xs font-medium text-slate-500">每日时刻</span>
          <input
            type="time"
            value={form.hhmm}
            onChange={(e) => onChange({ ...form, hhmm: e.target.value || '00:00' })}
            disabled={!form.enabled}
            className="input h-8 !w-28 text-sm"
          />
        </div>
      </div>
      <p className="mt-1.5 text-[11px] leading-4 text-slate-400">{desc}</p>
    </div>
  );
}

export default function BuddyAiSettings() {
  const pushToast = useAppStore((s) => s.pushToast);
  const settings = useAppStore((s) => s.settings);
  const refreshSettings = useAppStore((s) => s.refreshSettings);

  // 路径表单：输入框直显（自动检测预填，可人工替换），随右上角「保存配置」统一提交
  const [pathForm, setPathForm] = useState({ workbuddy_ai_path: '', codebuddy_ai_path: '' });
  const [locWb, setLocWb] = useState<AppLocate | null>(null);
  const [locCb, setLocCb] = useState<AppLocate | null>(null);
  // 任务表单（开关 + 时刻；存 app Settings，随「保存配置」统一提交）
  const [renewForm, setRenewForm] = useState<TaskForm>({ enabled: true, hhmm: '10:35' });
  const [checkinForm, setCheckinForm] = useState<TaskForm>({ enabled: false, hhmm: '09:20' });
  // 积分数据同步表单（五模式：每小时/每N小时/每N天/每日定时/关闭；存 app Settings）
  const [creditsForm, setCreditsForm] = useState({ mode: 'daily', hhmm: '23:45', hours: '2', days: '1' });
  const [detecting, setDetecting] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  // 设置快照只预填一次（保存后 refreshSettings 推新快照，不回灌正在编辑的表单）
  const prefilled = useRef(false);

  const LOCATE_SOURCE_LABEL: Record<string, string> = {
    settings: '人工指定',
    registry: '注册表',
    default: '默认路径',
    process: '运行进程',
    not_found: '未检测到',
  };

  const refreshLocate = useCallback(async () => {
    setRefreshing(true);
    try {
      const [w, c] = await Promise.all([
        api.env.locate('workbuddy_ai').catch(() => null),
        api.env.locate('codebuddy_ai').catch(() => null),
      ]);
      setLocWb(w);
      setLocCb(c);
    } finally {
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    void refreshLocate();
  }, [refreshLocate]);

  // 首次拿到设置快照：预填路径与任务表单（一次性）
  useEffect(() => {
    if (prefilled.current || !settings) return;
    prefilled.current = true;
    setPathForm({
      workbuddy_ai_path: settings.workbuddy_ai_path ?? '',
      codebuddy_ai_path: settings.codebuddy_ai_path ?? '',
    });
    setRenewForm({
      enabled: settings.buddy_ai_renew_enabled ?? true,
      hhmm: settings.buddy_ai_renew_hhmm || '10:35',
    });
    setCheckinForm({
      enabled: settings.buddy_ai_checkin_enabled === true,
      hhmm: settings.buddy_ai_checkin_hhmm || '09:20',
    });
    setCreditsForm({
      mode: settings.buddy_ai_credits_sync_mode || (settings.buddy_ai_credits_sync_enabled === false ? 'off' : 'daily'),
      hhmm: settings.buddy_ai_credits_sync_hhmm || '23:45',
      hours: settings.buddy_ai_credits_sync_hours || '2',
      days: settings.buddy_ai_credits_sync_days || '1',
    });
  }, [settings]);

  /** 自动检测：独立档案四级探测 → 命中即替换输入框内容，随「保存配置」统一生效 */
  const detectPath = async (target: 'workbuddy_ai' | 'codebuddy_ai') => {
    setDetecting(target);
    try {
      const r = await api.env.locate(target);
      if (target === 'workbuddy_ai') setLocWb(r);
      else setLocCb(r);
      if (r.exe) {
        const key = target === 'workbuddy_ai' ? 'workbuddy_ai_path' : 'codebuddy_ai_path';
        setPathForm((f) => ({ ...f, [key]: r.exe as string }));
        pushToast(
          'success',
          `已定位（${LOCATE_SOURCE_LABEL[r.source] ?? r.source}${r.version ? `，v${r.version}` : ''}）`,
        );
      } else {
        pushToast('warn', '未检测到客户端，请人工填写 exe 路径');
      }
    } catch (err) {
      pushToast('error', `检测失败：${String(err)}`);
    } finally {
      setDetecting(null);
    }
  };

  const save = async () => {
    setSaving(true);
    // 间隔边界夹取 + 回写表单（审查 P3：input min/max 只是提示，"0"/"99" 可提交；
    // 后端虽有 clamp 兜底，但表单显示值会与落库值不一致）
    const hours = String(Math.min(Math.max(Number(creditsForm.hours) || 2, 1), 24));
    const days = String(Math.min(Math.max(Number(creditsForm.days) || 1, 1), 30));
    const nextForm = { ...creditsForm, hours, days };
    setCreditsForm(nextForm);
    try {
      await api.misc.settingsSet({
        workbuddy_ai_path: pathForm.workbuddy_ai_path.trim() || null,
        codebuddy_ai_path: pathForm.codebuddy_ai_path.trim() || null,
        buddy_ai_renew_enabled: renewForm.enabled,
        buddy_ai_renew_hhmm: renewForm.hhmm || '10:35',
        buddy_ai_checkin_enabled: checkinForm.enabled,
        buddy_ai_checkin_hhmm: checkinForm.hhmm || '09:20',
        buddy_ai_credits_sync_mode: nextForm.mode,
        buddy_ai_credits_sync_hhmm: nextForm.hhmm || '23:45',
        buddy_ai_credits_sync_hours: hours,
        buddy_ai_credits_sync_days: days,
        buddy_ai_credits_sync_enabled: nextForm.mode !== 'off',
      });
      await refreshSettings();
      pushToast('success', 'BuddyAI 环境配置已保存');
    } catch (err) {
      pushToast('error', `保存失败：${String(err).slice(0, 120)}`);
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="animate-fade-in">
      <PageHeader
        title="BuddyAI · 环境配置"
        desc="通用配置 · 任务配置"
        actions={
          <>
            <button className="btn-outline" onClick={() => void refreshLocate()} disabled={refreshing}>
              <RefreshCw size={15} className={refreshing ? 'animate-spin' : ''} /> 重新检测
            </button>
            <button className="btn-outline" onClick={() => void save()} disabled={saving || !settings}>
              {saving ? <Spinner /> : <Save size={15} />} 保存配置
            </button>
          </>
        }
      />

      {/* 通用配置 + 任务配置：一行两列（镜像国内 Buddy 环境配置布局） */}
      <div className="grid items-start gap-4 lg:grid-cols-2">
        {/* 左：通用配置（应用环境：国际双客户端路径，自动检测 + 人工指定） */}
        <div className="card p-4">
          <div className="mb-3 flex items-center gap-2">
            <SlidersHorizontal size={16} className="text-emerald-500" />
            <h2 className="font-medium">通用配置</h2>
          </div>

          <div className="mb-1 flex items-center justify-between">
            <h3 className="font-medium">应用环境</h3>
            <div className="flex items-center gap-2">
              <Badge tone={locWb?.exe ? 'green' : 'red'}>
                WorkBuddy AI{locWb?.exe ? `已安装${locWb.version ? ` v${locWb.version}` : ''}` : '未检测到'}
              </Badge>
              <Badge tone={locCb?.exe ? 'green' : 'red'}>
                CodeBuddy AI{locCb?.exe ? `已安装${locCb.version ? ` v${locCb.version}` : ''}` : '未检测到'}
              </Badge>
            </div>
          </div>
          <div className="space-y-3 text-xs">
            {/* WorkBuddy AI 客户端路径（自动检测预填，可人工替换，随「保存配置」生效） */}
            <div className="rounded-lg border border-slate-100 p-3 dark:border-zinc-800">
              <div className="mb-2 flex items-center justify-between font-medium text-slate-500">
                WorkBuddy AI 客户端路径
                <Badge tone={locWb?.exe ? 'green' : 'amber'}>
                  {LOCATE_SOURCE_LABEL[locWb?.source ?? 'not_found']}
                </Badge>
              </div>
              <div className="flex items-center gap-2">
                <input
                  className="input flex-1 font-mono text-xs"
                  value={pathForm.workbuddy_ai_path}
                  onChange={(e) => setPathForm((f) => ({ ...f, workbuddy_ai_path: e.target.value }))}
                  placeholder={locWb?.exe ?? '未检测到，留空 = 自动检测'}
                />
                <button
                  className="btn-outline shrink-0 !px-2 !py-1"
                  onClick={() => void detectPath('workbuddy_ai')}
                  disabled={detecting !== null}
                >
                  {detecting === 'workbuddy_ai' ? <Spinner /> : <Search size={13} />} 自动检测
                </button>
              </div>
              <p className="mt-1.5 text-slate-400">
                国际版客户端（Programs\WorkBuddyAI，登录 workbuddy.ai）；路径改动随右上角「保存配置」生效，留空恢复自动检测
              </p>
            </div>
            {/* CodeBuddy AI 客户端路径 */}
            <div className="rounded-lg border border-slate-100 p-3 dark:border-zinc-800">
              <div className="mb-2 flex items-center justify-between font-medium text-slate-500">
                CodeBuddy AI 客户端路径
                <Badge tone={locCb?.exe ? 'green' : 'amber'}>
                  {LOCATE_SOURCE_LABEL[locCb?.source ?? 'not_found']}
                </Badge>
              </div>
              <div className="flex items-center gap-2">
                <input
                  className="input flex-1 font-mono text-xs"
                  value={pathForm.codebuddy_ai_path}
                  onChange={(e) => setPathForm((f) => ({ ...f, codebuddy_ai_path: e.target.value }))}
                  placeholder={locCb?.exe ?? '未检测到，留空 = 自动检测'}
                />
                <button
                  className="btn-outline shrink-0 !px-2 !py-1"
                  onClick={() => void detectPath('codebuddy_ai')}
                  disabled={detecting !== null}
                >
                  {detecting === 'codebuddy_ai' ? <Spinner /> : <Search size={13} />} 自动检测
                </button>
              </div>
              <p className="mt-1.5 text-slate-400">
                国际域登录态客户端（与国内 CodeBuddy 同目录）；路径改动随右上角「保存配置」生效，留空恢复自动检测
              </p>
            </div>
          </div>
        </div>

        {/* 右：任务配置（JWT Token 定时续期 / 每日自动打卡 / 积分数据同步，开关+时刻可改） */}
        <div className="card p-4">
          <div className="mb-3 flex items-center gap-2">
            <ListChecks size={16} className="text-emerald-500" />
            <h2 className="font-medium">任务配置</h2>
          </div>
          <div className="space-y-3">
            <TaskRow
              title="JWT Token 定时续期"
              desc="每日定时巡检，token 到期前 48h 内自动走 workbuddy.ai 刷新端点轮换（lazy 48h，未临期空转不计失败）；网关 401 亦会触发即时续期。"
              form={renewForm}
              onChange={setRenewForm}
            />
            <TaskRow
              title="每日自动打卡"
              desc="网页会话 + 沙箱跑一轮对话，服务端自动入账 30/50 积分（按套餐定档）；幂等跳过当日已完成账号；间隔等细项在「活跃打卡」页配置。"
              form={checkinForm}
              onChange={setCheckinForm}
            />
            {/* 积分数据同步（五模式对齐国内：每小时/每N小时/每N天/每日定时/关闭，随「保存配置」生效） */}
            <div className="rounded-lg border border-slate-100 p-3 dark:border-zinc-800">
              <div className="flex flex-wrap items-center gap-2 text-sm">
                <span className="font-medium">积分数据同步</span>
                <Badge tone={creditsForm.mode !== 'off' ? 'green' : 'amber'}>
                  {creditsForm.mode !== 'off' ? '已开启' : '已关闭'}
                </Badge>
                <span className="text-xs font-medium text-slate-500">同步模式</span>
                <select
                  className="input !h-9 !w-36 truncate !py-0 text-sm"
                  value={creditsForm.mode}
                  onChange={(e) => setCreditsForm({ ...creditsForm, mode: e.target.value })}
                >
                  <option value="every_hours">每N小时</option>
                  <option value="every_days">每N天</option>
                  <option value="hourly">每小时</option>
                  <option value="daily">每日定时</option>
                  <option value="off">关闭</option>
                </select>
                {creditsForm.mode === 'every_hours' && (
                  <>
                    <span className="text-xs font-medium text-slate-500">同步间隔</span>
                    <input
                      type="number"
                      min={1}
                      max={24}
                      className="input h-8 !w-16 text-sm"
                      value={creditsForm.hours}
                      onChange={(e) => setCreditsForm({ ...creditsForm, hours: e.target.value })}
                    />
                    <span className="text-xs text-slate-500">小时（1-24）</span>
                  </>
                )}
                {creditsForm.mode === 'every_days' && (
                  <>
                    <span className="text-xs font-medium text-slate-500">同步间隔</span>
                    <input
                      type="number"
                      min={1}
                      max={30}
                      className="input h-8 !w-16 text-sm"
                      value={creditsForm.days}
                      onChange={(e) => setCreditsForm({ ...creditsForm, days: e.target.value })}
                    />
                    <span className="text-xs text-slate-500">天（1-30）</span>
                  </>
                )}
                {creditsForm.mode === 'daily' && (
                  <>
                    <span className="text-xs font-medium text-slate-500">每日时刻</span>
                    <input
                      type="time"
                      value={creditsForm.hhmm}
                      onChange={(e) => setCreditsForm({ ...creditsForm, hhmm: e.target.value || '00:00' })}
                      className="input h-8 !w-28 text-sm"
                    />
                  </>
                )}
              </div>
              <p className="mt-1.5 text-[11px] leading-4 text-slate-400">
                应用运行期间按所选模式自动同步积分与 Token 看板数据（get-user-resource 全池取数 →
                积分快照 + Token 重扫 + 官网用量刷新）；间隔模式按「距上次成功执行满 N 小时/天」触发，
                无可用凭证时静默跳过。模式与间隔随右上角「保存配置」生效。
              </p>
            </div>
          </div>
          <div className="mt-3 rounded-md bg-slate-50 px-3 py-2 text-[11px] leading-4 text-slate-500 dark:bg-zinc-900 dark:text-zinc-400">
            任务开关与时刻改动随右上角「保存配置」统一生效；调度在应用运行期间执行（当天已过时刻、下次启动会自动补跑），应用关闭期间不执行。
            模型目录每日同步（默认 05:55）默认开启，开关在「资源调度」页。
          </div>
        </div>
      </div>
    </div>
  );
}
