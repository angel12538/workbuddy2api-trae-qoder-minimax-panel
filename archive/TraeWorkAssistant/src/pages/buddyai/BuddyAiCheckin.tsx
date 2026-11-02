import { useEffect, useMemo, useRef, useState } from 'react';
import { ChevronLeft, ChevronRight, Loader2, PlayCircle } from 'lucide-react';
import { listen } from '@tauri-apps/api/event';
import { Badge } from '../../components/ui';
import PageHeader from '../../components/PageHeader';
import { api } from '../../lib/tauri';
import { useAppStore } from '../../store';
import { withMinDelay } from '../../lib/delay';
import type { BuddyAiAccountView, WbCheckinRecord } from '../../types';

/**
 * BuddyAI · 活跃打卡页（布局对齐国内 Buddy「签到与成长」，以每日活跃打卡替代签到逻辑）。
 *
 * 一键打卡卡（账号表 + 底部说明左/「开始打卡」按钮右下，对齐国内一键签到卡）→
 * 实时进度卡（条件渲染：汇总徽标 + 逐账号结果行）→ 活动打卡日历（整行）。
 * 定时开关与时刻在「环境配置 → 任务配置」统一配置（与国内口径一致），本页仅保留
 * 账号间隔微调。国际版机制：网页会话 + 沙箱 ACP 六步链跑一轮对话，官方服务端
 * 自动入账 30/50 积分（无 claim 端点）。
 *
 * 数据源：buddy_ai_checkin_results（P2 落盘）+ buddy_ai_accounts_list 池快照。
 */
export default function BuddyAiCheckin() {
  const pushToast = useAppStore((s) => s.pushToast);
  const settings = useAppStore((s) => s.settings);
  const refreshSettings = useAppStore((s) => s.refreshSettings);
  const [gap, setGap] = useState(3);
  const [saving, setSaving] = useState(false);
  const [running, setRunning] = useState(false);
  const [lines, setLines] = useState<string[]>([]);
  // 整轮完成汇总徽标（对齐国内口径，用打卡用语：「完成：成功 x · 已打 y · 失败 z」）
  const [doneInfo, setDoneInfo] = useState<string | null>(null);
  const [accounts, setAccounts] = useState<BuddyAiAccountView[]>([]);
  const [records, setRecords] = useState<WbCheckinRecord[]>([]);
  // 日历当前月份（默认本月）
  const [calMonth, setCalMonth] = useState(() => {
    const n = new Date();
    return { y: n.getFullYear(), m: n.getMonth() };
  });

  const load = async () => {
    try {
      // 审查修复（L24）：原两路 .catch(()=>[]) 吞错，外层 catch 不可达，失败静默空态；
      // 改为逐路 catch 里 toast 报错后再回落空数组
      const [accs, recs] = await Promise.all([
        api.buddyai.accountsList().catch((err) => {
          pushToast('error', `账号列表加载失败：${String(err).slice(0, 120)}`);
          return [] as BuddyAiAccountView[];
        }),
        api.buddyai.checkinResults(90).catch((err) => {
          pushToast('error', `打卡记录加载失败：${String(err).slice(0, 120)}`);
          return [] as WbCheckinRecord[];
        }),
      ]);
      setAccounts(accs);
      setRecords(recs);
    } catch (err) {
      pushToast('error', `数据加载失败：${String(err).slice(0, 120)}`);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  };

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // 间隔回填（settings store 加载后一次性同步；开关/时刻归环境配置页）
  useEffect(() => {
    if (settings) {
      setGap(typeof settings.buddy_ai_checkin_gap_secs === 'number' ? settings.buddy_ai_checkin_gap_secs : 3);
    }
  }, [settings]);

  const saveGap = async () => {
    // FM-3 同源口径：间隔非法值夹取（0–600s）；未改动不重复保存（审查 P3：
    // onBlur 无条件保存会弹「已保存」噪音 + 多余 settingsSet）
    const cur = settings?.buddy_ai_checkin_gap_secs;
    if (gap === (typeof cur === 'number' ? cur : 3)) return;
    const nextGap = Number.isFinite(gap) ? Math.min(Math.max(Math.round(gap), 0), 600) : 0;
    setSaving(true);
    try {
      await withMinDelay(api.misc.settingsSet({ buddy_ai_checkin_gap_secs: nextGap }), 400);
      await refreshSettings();
      pushToast('success', '账号间隔已保存');
    } catch (err) {
      // FH-1 审查修复：保存失败回滚本地态（refreshSettings 触发回填 effect）
      await refreshSettings().catch(() => {});
      pushToast('error', `保存失败（已还原显示）：${String(err).slice(0, 120)}`);
    } finally {
      setSaving(false);
    }
  };

  // 打卡进度事件（NDJSON 契约对齐国内：payload 为序列化 JSON 字符串）。
  // 挂载期注册一次（审查 L3：异步注册晚于 invoke 会丢 start/首个账号事件）；
  // running=false 期间到达的事件（调度器/启动补跑触发）丢弃过程行，
  // 但 done/exit 仍静默刷新列表——定时任务跑完后表格需反映最新打卡状态
  const runningRef = useRef(false);
  useEffect(() => {
    runningRef.current = running;
  }, [running]);
  // 事件活动看门狗（审查 P2）：任一事件到达即重置计时；连续 240s 无事件视为
  // 事件链丢失（webview 通道抖动 / 线程异常退出且 exit 缺失），解锁按钮并提示。
  // 240s > 单账号最长静默段（180s 轮询 + 步骤余量），正常推进永不误触
  const wdRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const armWatchdog = () => {
    if (wdRef.current) clearTimeout(wdRef.current);
    wdRef.current = setTimeout(() => {
      if (!runningRef.current) return;
      setRunning(false);
      pushToast('warn', '打卡超过 240 秒无进度事件，已解除按钮锁定；结果以列表与日志为准');
    }, 240_000);
  };
  useEffect(() => {
    const un = listen<string>('buddy-ai-checkin-progress', (ev) => {
      let parsed: Record<string, unknown>;
      try {
        parsed = JSON.parse(ev.payload);
      } catch {
        return;
      }
      if (!runningRef.current) {
        if (parsed.type === 'done') void load();
        return;
      }
      armWatchdog();
      if (parsed.type === 'start') {
        setLines([`开始打卡（共 ${parsed.total} 个账号）…`]);
        setDoneInfo(null);
      } else if (parsed.type === 'done') {
        setLines((prev) => [
          ...prev,
          `整轮完成：成功 ${parsed.ok} / 已打 ${parsed.already} / 失败 ${parsed.failed}` +
            // FH-2 配套：初始化失败等异常路径的 done 带失败原因，必须可见
            (parsed.message ? ` — ${parsed.message}` : ''),
        ]);
        setDoneInfo(`完成：成功 ${parsed.ok} · 已打 ${parsed.already} · 失败 ${parsed.failed}`);
        if (Number(parsed.failed) > 0) {
          pushToast('error', `打卡存在失败（${parsed.failed} 个账号），详见执行进度`);
        }
        void load();
      } else if (parsed.type === 'progress') {
        // 过程心跳（账号开跑 / 轮询等待进度），长静默期可见
        setLines((prev) => [...prev, `${parsed.name ?? ''}：${parsed.message ?? ''}`]);
      } else if (parsed.type === 'exit') {
        setRunning(false);
      } else if (parsed.index) {
        setLines((prev) => [...prev, `#${parsed.index} ${parsed.name ?? ''}：${parsed.status ?? ''} — ${parsed.message ?? ''}`]);
      }
    });
    return () => {
      void un.then((f) => f());
      if (wdRef.current) clearTimeout(wdRef.current);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const startCheckin = async () => {
    if (running) return;
    setRunning(true);
    armWatchdog();
    setLines([]);
    try {
      await api.buddyai.checkinStart();
    } catch (err) {
      setRunning(false);
      pushToast('error', `发起打卡失败：${String(err).slice(0, 120)}`);
    }
  };

  // ── 统计口径 ──
  const localDateStr = (dt: Date) =>
    `${dt.getFullYear()}-${String(dt.getMonth() + 1).padStart(2, '0')}-${String(dt.getDate()).padStart(2, '0')}`;
  const today = localDateStr(new Date());

  // 今日每账号状态（records 终态优先，未打卡/停用兜底）
  const todayByUid = useMemo(() => {
    const m = new Map<string, WbCheckinRecord>();
    for (const r of records) if (r.date === today) m.set(r.user_id, r);
    return m;
  }, [records, today]);

  // 打卡日历（月历热图）：全成功=绿 / 部分成功=琥珀 / 有失败=玫瑰 / 无记录=灰
  const calCells = useMemo(() => {
    const first = new Date(calMonth.y, calMonth.m, 1);
    const daysInMonth = new Date(calMonth.y, calMonth.m + 1, 0).getDate();
    const lead = (first.getDay() + 6) % 7; // 周一为首列
    const cells: ({ day: number; date: string } | null)[] = Array.from({ length: lead }, () => null);
    for (let d = 1; d <= daysInMonth; d++) {
      cells.push({ day: d, date: `${calMonth.y}-${String(calMonth.m + 1).padStart(2, '0')}-${String(d).padStart(2, '0')}` });
    }
    return cells;
  }, [calMonth]);
  const dayStat = useMemo(() => {
    const m = new Map<string, { s: number; f: number; o: number }>();
    for (const r of records) {
      const v = m.get(r.date) ?? { s: 0, f: 0, o: 0 };
      if (r.status === 'success') v.s += 1;
      else if (r.status === 'fail') v.f += 1;
      else v.o += 1;
      m.set(r.date, v);
    }
    return m;
  }, [records]);

  const checkinEnabled = settings?.buddy_ai_checkin_enabled === true;
  const checkinHhmm = settings?.buddy_ai_checkin_hhmm?.trim() || '09:20';

  return (
    <div className="animate-fade-in">
      <PageHeader
        title="BuddyAI · 活跃打卡"
        desc="一键打卡 · 打卡日历 · 定时任务在环境配置页统一配置"
      />

      {/* 一键打卡卡（对齐国内一键签到卡：header 徽标行 + 圆角边框表格 + 按钮右下） */}
      <div className="card p-4">
        <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
          <div className="flex items-center gap-2">
            <span className="text-sm font-medium">一键打卡</span>
            {running && <Badge tone="blue">执行中</Badge>}
            {checkinEnabled ? (
              <Badge tone="green">每日 {checkinHhmm} 自动执行</Badge>
            ) : (
              <Badge tone="amber">定时未开启</Badge>
            )}
          </div>
          {accounts.length > 0 && !running && (
            <span className="text-xs text-slate-400">已打卡账号自动跳过</span>
          )}
        </div>

        <div className="overflow-x-auto rounded-lg border border-slate-200 dark:border-zinc-700">
          <table className="w-full min-w-[680px] text-sm">
            <thead className="bg-slate-50 text-xs text-slate-500 dark:bg-zinc-900">
              <tr>
                <th className="px-3 py-1.5 text-left">账号</th>
                <th className="px-3 py-1.5 text-left">打卡状态</th>
                <th className="px-3 py-1.5 text-left">完成时间</th>
                <th className="px-3 py-1.5 text-right">获取积分</th>
                <th className="px-3 py-1.5 text-right">可用总积分</th>
              </tr>
            </thead>
            <tbody>
              {accounts.length === 0 ? (
                <tr>
                  <td colSpan={5} className="px-3 py-4 text-center text-xs text-slate-400">
                    暂无国际账号：先到「账号管理」OAuth登录或导入账号
                  </td>
                </tr>
              ) : (
                accounts.map((a) => {
                  const rec = todayByUid.get(a.id);
                  return (
                    <tr key={a.id} className="border-t border-slate-100 dark:border-zinc-800">
                      <td className="px-3 py-1.5">
                        <div className="font-medium">{a.nickname || a.id}</div>
                        <div className="text-xs text-slate-400">{a.uid || a.id}</div>
                      </td>
                      <td className="px-3 py-1.5">
                        {a.disabled ? (
                          <Badge tone="slate">已停用</Badge>
                        ) : rec?.status === 'success' ? (
                          <Badge tone="green">已打</Badge>
                        ) : rec?.status === 'already' ? (
                          <Badge tone="blue">已打（本轮已打）</Badge>
                        ) : rec?.status === 'fail' ? (
                          <Badge tone="red">失败</Badge>
                        ) : !a.has_credential ? (
                          <Badge tone="amber">无凭证</Badge>
                        ) : a.needs_relogin ? (
                          <Badge tone="red">需重登</Badge>
                        ) : (
                          <Badge tone="slate">未打</Badge>
                        )}
                      </td>
                      <td className="px-3 py-1.5 text-xs tabular-nums text-slate-500">{rec?.time || '—'}</td>
                      <td className="px-3 py-1.5 text-right tabular-nums text-xs">
                        {rec?.reward != null ? (
                          <span className="text-emerald-600 dark:text-emerald-400">+{rec.reward.toFixed(0)}</span>
                        ) : (
                          <span className="text-slate-300 dark:text-zinc-600">—</span>
                        )}
                      </td>
                      <td className="px-3 py-1.5 text-right tabular-nums text-xs">
                        {a.credits_balance != null ? a.credits_balance.toFixed(2) : '-'}
                      </td>
                    </tr>
                  );
                })
              )}
            </tbody>
          </table>
        </div>

        {/* 操作区：说明文字左（含账号间隔微调）、开始打卡按钮右下（对齐国内） */}
        <div className="mt-3 flex flex-wrap items-center justify-between gap-3">
          <div className="flex flex-wrap items-center gap-2 text-xs text-slate-400">
            <span>
              {running
                ? '打卡进行中，逐账号结果见下方实时进度…'
                : '幂等：每账号当日成功打卡一次，重复触发自动跳过；免费模型跑轮，不消耗付费额度'}
            </span>
            <label className="flex items-center gap-1.5 text-slate-500">
              账号间隔
              <input
                type="number"
                min={0}
                max={600}
                className="w-16 rounded-md border border-slate-200 bg-transparent px-2 py-1 text-right text-xs tabular-nums focus:border-brand-400 focus:outline-none dark:border-zinc-700 dark:bg-zinc-900"
                value={gap}
                onChange={(e) => setGap(Number(e.target.value) || 0)}
                onBlur={() => void saveGap()}
                disabled={saving}
              />
              秒
            </label>
          </div>
          <button
            className="btn-outline"
            disabled={running || accounts.length === 0}
            onClick={() => void startCheckin()}
          >
            {running ? <Loader2 size={15} className="animate-spin" /> : <PlayCircle size={15} />}
            {running ? '打卡中…' : '开始打卡'}
          </button>
        </div>
      </div>

      {/* 实时进度卡（条件渲染，对齐国内：汇总徽标 + 逐账号结果行） */}
      {lines.length > 0 && (
        <div className="card mt-4 p-4">
          <div className="mb-3 flex items-center justify-between">
            <h3 className="font-medium">实时进度</h3>
            <div className="flex items-center gap-3">
              {running ? (
                <Badge tone="blue">运行中</Badge>
              ) : doneInfo ? (
                <Badge tone={doneInfo.includes('失败 0') ? 'green' : 'amber'}>{doneInfo}</Badge>
              ) : (
                <Badge tone="slate">已结束</Badge>
              )}
            </div>
          </div>
          <div className="max-h-56 space-y-1 overflow-y-auto rounded-md bg-slate-50 p-3 font-mono text-[11px] leading-5 text-slate-600 dark:bg-zinc-900 dark:text-zinc-300">
            {lines.map((l, i) => (
              <div key={i}>{l}</div>
            ))}
          </div>
        </div>
      )}

      {/* 活动打卡日历（整行；对齐国内「活动签到日历」交互：月历热图 + 月份切换） */}
      <div className="card mt-4 p-4">
        <div className="mb-3 flex items-center justify-between">
          <span className="text-sm font-medium">活动打卡日历</span>
          <div className="flex items-center gap-1">
            <button
              className="btn-ghost p-1"
              onClick={() =>
                setCalMonth((prev) => (prev.m === 0 ? { y: prev.y - 1, m: 11 } : { y: prev.y, m: prev.m - 1 }))
              }
            >
              <ChevronLeft size={15} />
            </button>
            <span className="text-xs tabular-nums text-slate-500">
              {calMonth.y} 年 {calMonth.m + 1} 月
            </span>
            <button
              className="btn-ghost p-1"
              onClick={() =>
                setCalMonth((prev) => (prev.m === 11 ? { y: prev.y + 1, m: 0 } : { y: prev.y, m: prev.m + 1 }))
              }
            >
              <ChevronRight size={15} />
            </button>
          </div>
        </div>
        <div className="grid grid-cols-7 gap-1 text-center text-[11px] text-slate-400">
          {['一', '二', '三', '四', '五', '六', '日'].map((d) => (
            <div key={d}>{d}</div>
          ))}
          {calCells.map((c, i) => {
            if (!c) return <div key={`e${i}`} />;
            const st = dayStat.get(c.date);
            // FM-4 审查修复：有失败优先标玫瑰（原实现「1 成功 + 2 失败」显示绿色
            // 与图例矛盾）；全无失败且有成功=翡翠；仅跳过/已完成=琥珀
            const cls = st
              ? st.f > 0
                ? 'bg-rose-500/15 text-rose-500'
                : st.s > 0
                  ? 'bg-emerald-500/15 text-emerald-500'
                  : 'bg-amber-500/15 text-amber-500'
              : 'text-slate-400';
            return (
              <div key={c.date} className={`rounded-md px-1 py-1.5 ${cls}`} title={st ? `成功 ${st.s} · 失败 ${st.f}` : '无打卡记录'}>
                <div className="tabular-nums">{c.day}</div>
                {st ? (
                  <div className="mt-0.5 flex justify-center gap-0.5">
                    {st.s > 0 && <span className="h-1 w-1 rounded-full bg-emerald-500" />}
                    {st.o > 0 && <span className="h-1 w-1 rounded-full bg-amber-500" />}
                    {st.f > 0 && <span className="h-1 w-1 rounded-full bg-rose-500" />}
                  </div>
                ) : null}
              </div>
            );
          })}
        </div>
        <p className="mt-2 text-[11px] leading-4 text-slate-400">
          <span className="text-emerald-500">●</span> 当日有打卡成功　
          <span className="text-amber-500">●</span> 仅跳过/已完成　
          <span className="text-rose-500">●</span> 有失败
        </p>
      </div>
    </div>
  );
}
