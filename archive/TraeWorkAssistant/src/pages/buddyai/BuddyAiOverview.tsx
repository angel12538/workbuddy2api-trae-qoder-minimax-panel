import { useEffect, useMemo, useState } from 'react';
import { RefreshCw, CheckCircle2, Circle, ChevronRight, ExternalLink, Loader2 } from 'lucide-react';
import {
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  LabelList,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import { Badge, StatCard } from '../../components/ui';
import PageHeader from '../../components/PageHeader';
import { api } from '../../lib/tauri';
import { useAppStore } from '../../store';
import { useIsDark } from '../../lib/useIsDark';
import type { AppLocate, BuddyAiAccountView, ViewKey, WbCheckinRecord } from '../../types';

/**
 * BuddyAI · 概述页（六页子导航对齐国内 Buddy「概述」形态）。
 *
 * 顶部统计卡（账号总数 / 可用总积分 / 登录账号 / 本机套餐 / 凭证告警，镜像国内五卡）+
 * 配置导航（安装客户端 → 扫描本机账号 → 首次打卡 → 查询积分 → 国际档调度[可选]）。
 * 数据全部复用 buddy_ai_* 独立池命令，与国内 Buddy 概述零数据共享；
 * 模型目录统一在「资源调度」页处理。
 */

/** 配置导航步骤（对齐国内 Buddy 概述 SetupGuide 形态；optional 步骤不计入完成度） */
interface Step {
  key: string;
  title: string;
  desc: string;
  done: boolean;
  actionLabel: string;
  view: ViewKey;
  /** 可选步骤：不计入 x/y 完成度（如国际档调度） */
  optional?: boolean;
}

export default function BuddyAiOverview() {
  const pushToast = useAppStore((s) => s.pushToast);
  const setView = useAppStore((s) => s.setView);
  const settings = useAppStore((s) => s.settings);
  const proxy = useAppStore((s) => s.proxy);
  const isDark = useIsDark();
  const [accounts, setAccounts] = useState<BuddyAiAccountView[]>([]);
  const [records, setRecords] = useState<WbCheckinRecord[]>([]);
  const [intlEnabled, setIntlEnabled] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [wbLocate, setWbLocate] = useState<AppLocate | null>(null);
  const [openingClient, setOpeningClient] = useState(false);
  // 本机登录账号（account-snapshot.json：editionType = 本机套餐真实口径）
  const [localAcct, setLocalAcct] = useState<{ nickname: string; edition_type: string; is_pro: boolean } | null>(null);

  const refresh = async () => {
    setRefreshing(true);
    try {
      const [accs, recs, pf] = await Promise.all([
        api.buddyai.accountsList().catch((e) => {
          pushToast('error', `账号列表加载失败：${String(e).slice(0, 120)}`);
          return [] as BuddyAiAccountView[];
        }),
        api.buddyai.checkinResults(30).catch(() => [] as WbCheckinRecord[]),
        api.apiServer.poolList().catch((e) => {
          // FM-1：加载失败与「未开启」必须可区分（原静默吞掉显示假态）
          pushToast('error', `池配置加载失败：${String(e).slice(0, 120)}`);
          return null;
        }),
      ]);
      setAccounts(accs);
      setRecords(recs);
      setIntlEnabled(pf?.buddy_ai_enabled ?? false);
      api.buddyai
        .localScan()
        .then((r) => {
          if (r.found) setLocalAcct({ nickname: r.nickname ?? '', edition_type: r.edition_type ?? '', is_pro: r.is_pro ?? false });
        })
        .catch(() => {});
    } finally {
      setRefreshing(false);
    }
  };

  useEffect(() => {
    void refresh();
    api.env.locate('workbuddy_ai').then((r) => setWbLocate(r)).catch(() => {});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // 打开国际客户端（对齐国内概述「打开客户端」：代理运行中注入 --proxy-server）
  const openClientAi = () => {
    setOpeningClient(true);
    api.env
      .openWorkbuddyAiApp(proxy.running ? proxy.port : undefined)
      .then(() => pushToast('success', '已启动 WorkBuddy AI'))
      .catch((e) => pushToast('error', `打开客户端失败：${String(e)}`))
      .finally(() => setOpeningClient(false));
  };

  const total = accounts.length;
  const usable = accounts.filter((a) => a.has_credential && !a.needs_relogin).length;
  const withBalance = accounts.filter((a) => a.credits_balance != null);
  const totalBalance = withBalance.reduce((s, a) => s + (a.credits_balance ?? 0), 0);
  // 凭证告警：需重新登录 / 无凭证 / token 24h 内将过期（含已过期）+ 积分包 7 日内将过期包数（仍有剩余，镜像国内口径）
  const nowSec = Math.floor(Date.now() / 1000);
  const tokenSoon = accounts.filter(
    (a) => a.has_credential && a.access_token_expires_at != null && a.access_token_expires_at <= nowSec + 86400,
  ).length;
  const pkgSoon = accounts.reduce(
    (n, a) =>
      n +
      (a.credits_packages ?? []).filter(
        // 与积分看板 pkgSoon 同口径：未来 7 天窗口（补 > nowSec 下界——
        // 原判定把已过期包也计入告警，会持续挂在提醒卡上）
        (p) =>
          p.expire_secs != null && p.expire_secs > nowSec && p.expire_secs <= nowSec + 7 * 86400 && p.remain > 0,
      ).length,
    0,
  );
  const alertCount =
    accounts.filter((a) => !a.has_credential || a.needs_relogin).length + tokenSoon + pkgSoon;
  const checkinEnabled = settings?.buddy_ai_checkin_enabled === true;
  const creditsFetched = withBalance.length > 0;

  // 登录账号 / 本机套餐（对齐国内概述口径）：国际版无 edition_type 数据源，
  // 登录账号 = 凭证可用账号数；本机套餐 = 积分包名聚合（打卡入账 30/50 按套餐定档）
  const pkgNames = useMemo(
    () =>
      [
        ...new Set(
          accounts.flatMap((a) => (a.credits_packages ?? []).map((p) => p.name).filter(Boolean)),
        ),
      ].slice(0, 3),
    [accounts],
  );
  const today = new Date().toLocaleDateString('sv-SE');

  // 配置导航（步骤完成态实时判定；镜像国内五步：安装 → 扫描本机账号 → 首签 → 积分 → 调度[可选]）
  const steps: Step[] = [
    {
      key: 'client',
      title: '安装 WorkBuddy AI 客户端',
      desc: '主客户端：打卡/积分/调度全功能。路径未自动识别时可在「环境配置」人工指定。',
      done: !!wbLocate?.exe,
      actionLabel: wbLocate?.exe ? '打开客户端' : '去配置',
      view: 'buddyai-settings',
    },
    {
      key: 'account',
      /* 审查修复（第7项残留复查）：docker 版已裁剪「扫描本机账号」入口，卡片标题与
         引导同步改为实际可用入口（OAuth 登录 / 导入账号），与 HelpModal 口径一致 */
      title: '添加账号',
      desc: '到「账号管理」用 workbuddy.ai OAuth 登录授权入池，或「导入账号」导入账号池 JSON（.workbuddy.ai 域凭证）。',
      done: total > 0,
      actionLabel: '去导入',
      view: 'buddyai-accounts',
    },
    {
      key: 'checkin',
      title: '完成首次打卡',
      desc: '验证打卡链路是否跑通（凭证有效、接口可达），成功才落幂等戳。',
      done: records.some((r) => r.date === today && r.status !== 'fail'),
      actionLabel: '去打卡',
      view: 'buddyai-checkin',
    },
    {
      key: 'credits',
      title: '查询积分余额',
      desc: '录入凭证后查询各账号积分包余额与到期情况，看板展示趋势。',
      done: creditsFetched,
      actionLabel: '查看积分',
      view: 'buddyai-credits',
    },
    {
      key: 'dispatch',
      title: '开启国际档调度（可选）',
      desc: 'buddy-ai 独立池参与统一网关路由，与国内 Buddy 档平级、互不影响。',
      done: intlEnabled,
      actionLabel: '去开启',
      view: 'buddyai-api-service',
      optional: true,
    },
  ];
  // 可选步骤不计入完成度（未开调度不阻塞「全部完成」）
  const required = steps.filter((s) => !s.optional);
  const completed = required.filter((s) => s.done).length;
  const allDone = completed === required.length;

  // 近 30 天打卡趋势（P2 数据源：buddy_ai_checkin_results 表；缺日补零，镜像国内堆叠口径）
  const localDateStr = (dt: Date) =>
    `${dt.getFullYear()}-${String(dt.getMonth() + 1).padStart(2, '0')}-${String(dt.getDate()).padStart(2, '0')}`;
  const trend = useMemo(() => {
    const days: string[] = [];
    for (let i = 29; i >= 0; i--) days.push(localDateStr(new Date(Date.now() - i * 86400000)));
    const byDate = new Map<string, { s: number; a: number; f: number }>();
    for (const r of records) {
      const d = byDate.get(r.date) ?? { s: 0, a: 0, f: 0 };
      if (r.status === 'success') d.s += 1;
      else if (r.status === 'already') d.a += 1;
      else if (r.status === 'fail') d.f += 1;
      byDate.set(r.date, d);
    }
    return days.map((d) => {
      const v = byDate.get(d) ?? { s: 0, a: 0, f: 0 };
      return { date: d.slice(5), 成功: v.s, 已完成: v.a, 失败: v.f };
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [records]);

  // 积分榜 Top（按余额降序 Top 10，对齐国内纵向柱图 + 数值标签口径）
  const topCredits = useMemo(
    () =>
      accounts
        .filter((a) => a.credits_balance != null && (a.credits_balance ?? 0) > 0)
        .sort((x, y) => (y.credits_balance ?? 0) - (x.credits_balance ?? 0))
        .slice(0, 10)
        .map((a) => ({
          name: (a.nickname || a.id).slice(0, 10),
          credits: Number((a.credits_balance ?? 0).toFixed(2)),
        })),
    [accounts],
  );

  // 今日打卡状态提示（调度开启 + 时刻；历史结果见下方近 30 天趋势图）
  const checkinHint = useMemo(
    () => (checkinEnabled ? `每日 ${settings?.buddy_ai_checkin_hhmm?.trim() || '09:20'} 自动执行` : '未开启定时'),
    [checkinEnabled, settings?.buddy_ai_checkin_hhmm],
  );

  return (
    <div className="animate-fade-in">
      <PageHeader
        title="BuddyAI · 概述"
        desc="Buddy 国际版（workbuddy.ai）运行总览 · 独立池/独立记账 · 打卡调度与积分"
        actions={
          <button onClick={() => void refresh()} className="btn-outline" disabled={refreshing}>
            <RefreshCw size={15} className={refreshing ? 'animate-spin' : ''} /> 刷新
          </button>
        }
      />

      {/* 顶部统计卡（镜像国内五卡：账号总数/可用总积分/登录账号/本机套餐/告警提醒） */}
      <div className="grid grid-cols-2 gap-3 md:grid-cols-5">
        <StatCard label="账号总数" value={total} hint={checkinHint} tone="brand" />
        <StatCard
          label="可用总积分"
          value={withBalance.length ? totalBalance.toFixed(2) : '—'}
          hint={withBalance.length ? `${withBalance.length}/${total} 个已取数` : '到「积分看板」同步后展示'}
          tone="amber"
        />
        <StatCard
          label="登录账号"
          value={usable > 0 ? usable : '未登录'}
          hint={`凭证可用 ${usable}/${total}，可参与打卡与调度`}
          tone={usable > 0 ? 'violet' : 'slate'}
        />
        <StatCard
          label="本机套餐"
          value={localAcct ? (localAcct.edition_type || 'free') + (localAcct.is_pro ? '（Pro）' : '') : pkgNames[0] ?? '—'}
          hint={
            localAcct
              ? `本机登录：${localAcct.nickname || '—'}（account-snapshot 实抓）`
              : pkgNames.length > 1
                ? `积分包含 ${pkgNames.join(' / ')}`
                // 审查修复（第7项残留复查）：扫描入口已裁剪，指向改为实际存在的 OAuth登录 按钮
                : '未检测到本机登录（账号管理 → OAuth登录）'
          }
          tone="violet"
        />
        <StatCard
          label="凭证告警"
          value={alertCount}
          hint={`需重登/无凭证 ${accounts.filter((a) => !a.has_credential || a.needs_relogin).length} · token 24h 内过期 ${tokenSoon} · 积分包 7 日内过期 ${pkgSoon}`}
          tone={alertCount > 0 ? 'red' : 'slate'}
        />
      </div>

      {/* 近 30 天打卡结果（镜像国内 Buddy 概述堆叠图；数据源 buddy_ai_checkin_results） */}
      <div className="card mt-5 p-4">
        <div className="mb-3 flex items-center justify-between">
          <span className="text-sm font-medium">近 30 天打卡结果</span>
          <span className="flex items-center gap-3 text-[11px] text-slate-400">
            <span className="inline-flex items-center gap-1"><span className="h-2 w-2 rounded-full bg-emerald-500" />成功</span>
            <span className="inline-flex items-center gap-1"><span className="h-2 w-2 rounded-full bg-sky-500" />已完成</span>
            <span className="inline-flex items-center gap-1"><span className="h-2 w-2 rounded-full bg-rose-500" />失败</span>
          </span>
        </div>
        <div className="h-52">
          <ResponsiveContainer width="100%" height="100%">
            <BarChart data={trend} margin={{ top: 4, right: 8, bottom: 0, left: -20 }}>
              <CartesianGrid strokeDasharray="3 3" stroke="#8884" vertical={false} />
              <XAxis dataKey="date" tick={{ fontSize: 10 }} interval={2} tickLine={false} axisLine={false} />
              <YAxis allowDecimals={false} tick={{ fontSize: 10 }} tickLine={false} axisLine={false} />
              <Tooltip cursor={{ fill: '#8881' }} contentStyle={{ fontSize: 12, borderRadius: 8 }} />
              <Bar dataKey="成功" stackId="r" fill="#10b981" />
              <Bar dataKey="已完成" stackId="r" fill="#0ea5e9" />
              <Bar dataKey="失败" stackId="r" fill="#f43f5e" radius={[3, 3, 0, 0]} />
            </BarChart>
          </ResponsiveContainer>
        </div>
      </div>

      {/* 积分榜 Top 榜（对齐国内：纵向柱图 Top10 + 数值标签 + 前三高亮） */}
      <div className="card mt-5 p-4">
        <div className="mb-3 flex items-center justify-between">
          <div className="flex items-center gap-2">
            <span className="text-sm font-medium">积分榜 Top 榜</span>
            {topCredits.length > 0 && (
              <span className="rounded-full bg-zinc-100 px-2 py-0.5 text-xs font-medium text-zinc-500 dark:bg-zinc-800 dark:text-zinc-400">
                Top {topCredits.length}
              </span>
            )}
          </div>
          <span className="text-[11px] text-slate-400">按可用积分排序</span>
        </div>
        {topCredits.length === 0 ? (
          <p className="py-6 text-center text-xs text-slate-400">暂无积分数据，先到「积分看板」同步。</p>
        ) : (
          <div className="h-64">
            <ResponsiveContainer width="100%" height="100%">
              <BarChart data={topCredits} margin={{ top: 16, right: 8, bottom: 0, left: -16 }}>
                <CartesianGrid strokeDasharray="3 3" stroke="#8884" vertical={false} />
                <XAxis dataKey="name" tick={{ fontSize: 10 }} interval={0} angle={-20} textAnchor="end" height={50} tickLine={false} axisLine={false} />
                <YAxis allowDecimals={false} tick={{ fontSize: 10 }} tickLine={false} axisLine={false} />
                <Tooltip cursor={{ fill: '#8881' }} contentStyle={{ fontSize: 12, borderRadius: 8 }} />
                <Bar dataKey="credits" name="可用积分" radius={[8, 8, 0, 0]} maxBarSize={44}>
                  {topCredits.map((_, i) => {
                    const colors = isDark
                      ? ['#fafafa', '#e4e4e7', '#d4d4d8']
                      : ['#27272a', '#3f3f46', '#52525b'];
                    const fill = i < 3 ? colors[i] : isDark
                      ? `rgba(212,212,216,${Math.max(0.35, 0.6 - (i - 3) * 0.05).toFixed(2)})`
                      : `rgba(82,82,91,${Math.max(0.35, 0.6 - (i - 3) * 0.05).toFixed(2)})`;
                    return <Cell key={i} fill={fill} />;
                  })}
                  <LabelList
                    dataKey="credits"
                    position="top"
                    formatter={(v: number) => (v >= 1000 ? `${(v / 1000).toFixed(1)}k` : v.toFixed(0))}
                    style={{ fontSize: 10, fill: isDark ? '#a1a1aa' : '#94a3b8', fontWeight: 500 }}
                  />
                </Bar>
              </BarChart>
            </ResponsiveContainer>
          </div>
        )}
      </div>

      {/* 配置导航 */}
      <div className="card mt-5 overflow-hidden">
        <div className="flex items-center justify-between border-b border-slate-100 px-4 py-3 dark:border-zinc-800">
          <div>
            <h3 className="font-medium">配置导航</h3>
            <p className="text-xs text-slate-500">按步骤完成初始化，已完成的步骤无需重复处理。</p>
          </div>
          <Badge tone={allDone ? 'green' : 'amber'}>
            {/* 审查修复（U 系列）：分子只计必选步骤，分母同步改为 required.length——
                原分母 steps.length 含可选步骤，未开调度时显示「4/5 已完成」却配
                绿色 allDone 徽标，完成度口径自相矛盾 */}
            {completed}/{required.length} 已完成
          </Badge>
        </div>
        <ol className="divide-y divide-slate-100 dark:divide-slate-800">
          {steps.map((step, i) => (
            <li key={step.key} className="flex items-center gap-3 px-4 py-3">
              <div className={step.done ? 'text-emerald-500' : 'text-slate-300 dark:text-zinc-600'}>
                {step.done ? <CheckCircle2 size={20} /> : <Circle size={20} />}
              </div>
              <div className="min-w-0 flex-1">
                <div className="text-sm font-medium text-slate-800 dark:text-zinc-100">
                  {i + 1}. {step.title}
                </div>
                <div className="text-xs text-slate-500">{step.desc}</div>
              </div>
              {step.done ? (
                <span className="shrink-0 rounded-full bg-emerald-50 px-2.5 py-1 text-xs font-medium text-emerald-600 dark:bg-emerald-500/15 dark:text-emerald-400">
                  已完成
                </span>
              ) : (
                <button
                  onClick={() => (step.key === 'client' && wbLocate?.exe ? openClientAi() : setView(step.view))}
                  className="btn-outline shrink-0"
                  disabled={step.key === 'client' && openingClient}
                >
                  {step.key === 'client' && openingClient ? <Loader2 size={14} className="animate-spin" /> : null}
                  {step.key === 'client' && wbLocate?.exe ? <ExternalLink size={14} /> : null}
                  {step.actionLabel}
                  <ChevronRight size={14} />
                </button>
              )}
            </li>
          ))}
        </ol>
        {allDone && (
          <div className="border-t border-slate-100 bg-emerald-50/60 px-4 py-3 text-sm text-emerald-700 dark:border-zinc-800 dark:bg-emerald-500/10 dark:text-emerald-300">
            全部配置已完成，每日打卡与积分同步交给自动化即可！
          </div>
        )}
      </div>
    </div>
  );
}
