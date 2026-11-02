import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { RefreshCw, Coins } from 'lucide-react';
import {
  Bar,
  CartesianGrid,
  ComposedChart,
  Legend,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import ActivityHeatmap from '../../components/charts/ActivityHeatmap';
import ExpiryCalendar, { type ExpiryItem } from '../../components/ExpiryCalendar';
import { SOURCES, SOURCE_LABELS, type BoardSource } from '../../components/ChartFilterBar';
import { StatCard, Badge } from '../../components/ui';
import PageHeader from '../../components/PageHeader';
import { api } from '../../lib/tauri';
import { useAppStore } from '../../store';
import { withMinDelay } from '../../lib/delay';
import type {
  BuddyAiAccountView,
  BuddyAiTokenStats,
  BuddyAiUsageOfficialAll,
  GroupView,
  UsageDayView,
  UsageSessionsResult,
  WbCheckinRecord,
} from '../../types';

/** P8 积分快照行（buddy_ai_credits_history 表） */
interface CreditSnap {
  date: string;
  ts: number;
  total_balance: number;
  accounts: Array<{ id: string; name: string; balance: number | null }>;
  earned: number | null;
}

/**
 * BuddyAI · 积分看板页（六页子导航对齐国内 Buddy）。
 *
 * 数据源：buddy_ai_accounts_list 池快照三键（credits_balance / credits_expire_at /
 * credits_fetched_at，由每日打卡前置快照与积分同步回写）；「刷新数据」触发
 * buddy_ai_credits_fetch 全池 get-user-resource 实时取数。
 * Intl 上游无积分包明细接口，按 TotalDosage 总额展示（对齐 §4.5 积分同步口径）。
 */

/** 多选下拉（移植国内 Dashboard 同款：chips 触发 + 面板勾选 + 搜索 + 清空） */
function MultiSelectDropdown({
  label,
  options,
  selected,
  onChange,
  width = 'w-56',
}: {
  label: string;
  options: { id: string; name: string; color?: string }[];
  selected: string[];
  onChange: (ids: string[]) => void;
  width?: string;
}) {
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState('');
  const ref = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDoc);
    return () => document.removeEventListener('mousedown', onDoc);
  }, [open]);
  const toggle = (id: string) =>
    onChange(selected.includes(id) ? selected.filter((x) => x !== id) : [...selected, id]);
  const kw = search.trim().toLowerCase();
  const visible = kw
    ? options.filter((o) => o.name.toLowerCase().includes(kw) || o.id.toLowerCase().includes(kw))
    : options;
  return (
    <div ref={ref} className="relative">
      <button
        type="button"
        className={`btn-outline !py-1.5 !text-xs ${selected.length ? '!border-sky-400 text-sky-600 dark:text-sky-400' : ''}`}
        onClick={() => {
          setOpen((v) => !v);
          setSearch('');
        }}
      >
        {label}
        {selected.length > 0 && (
          <span className="ml-1 rounded-full bg-sky-500 px-1.5 text-[10px] font-medium text-white">
            {selected.length}
          </span>
        )}
      </button>
      {open && (
        <div
          className={`absolute right-0 z-30 mt-1 max-h-72 ${width} overflow-auto rounded-lg border border-slate-200 bg-white p-2 shadow-lg dark:border-zinc-700 dark:bg-zinc-900`}
        >
          {options.length > 5 && (
            <input
              autoFocus
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder={`搜索${label}…`}
              className="input mb-1 !py-1 !text-xs"
            />
          )}
          {visible.length === 0 && (
            <div className="px-2 py-1 text-xs text-slate-400">
              {options.length === 0 ? '暂无可选项' : '无匹配项'}
            </div>
          )}
          {visible.map((o) => (
            <label
              key={o.id}
              className="flex cursor-pointer items-center gap-2 rounded px-2 py-1 text-xs hover:bg-slate-50 dark:hover:bg-zinc-800"
            >
              <input
                type="checkbox"
                checked={selected.includes(o.id)}
                onChange={() => toggle(o.id)}
                className="h-3.5 w-3.5"
              />
              {o.color && (
                <span className="inline-block h-2.5 w-2.5 shrink-0 rounded-full" style={{ background: o.color }} />
              )}
              <span className="truncate">{o.name}</span>
            </label>
          ))}
          {selected.length > 0 && (
            <button
              type="button"
              onClick={() => onChange([])}
              className="mt-1 w-full rounded px-2 py-1 text-left text-xs text-slate-400 hover:bg-slate-50 dark:hover:bg-zinc-800"
            >
              清空筛选（全部）
            </button>
          )}
        </div>
      )}
    </div>
  );
}

/** 共享空 Map（网关源无获得数据时的占位，避免 useMemo 依赖每次新建） */
const EMPTY_MAP: Map<string, number> = new Map();

/** 会话明细日期串（本地自然日，n 天前；0 = 今天） */
function sessDate(daysAgo: number): string {
  const d = new Date();
  d.setDate(d.getDate() - daysAgo);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

export default function BuddyAiCredits() {
  const pushToast = useAppStore((s) => s.pushToast);
  const [accounts, setAccounts] = useState<BuddyAiAccountView[] | null>(null);
  const [records, setRecords] = useState<WbCheckinRecord[]>([]);
  const [usage, setUsage] = useState<UsageDayView[]>([]);
  const [history, setHistory] = useState<CreditSnap[]>([]);
  const [loading, setLoading] = useState(true);
  const [syncing, setSyncing] = useState(false);
  // 会话明细（对齐国内 Buddy SessionsTab：定时落地的请求级行缓存，纯缓存零网络查询）
  const [sessStart, setSessStart] = useState(() => sessDate(6));
  const [sessEnd, setSessEnd] = useState(() => sessDate(0));
  const [sessModel, setSessModel] = useState('');
  const [sessUser, setSessUser] = useState('');
  const [sessPage, setSessPage] = useState(1);
  const [sessData, setSessData] = useState<UsageSessionsResult | null>(null);
  const [sessLoading, setSessLoading] = useState(false);
  // 模型下拉并集（对齐国内审查 m-7：过滤后子集不收缩下拉，跨查询累计）
  const [sessModels, setSessModels] = useState<string[]>([]);
  // 落地缓存重查键：积分同步/官网聚合刷新完成后 +1
  const [sessReload, setSessReload] = useState(0);
  // 全局筛选（对齐国内看板顶部分组/账号筛选：多选下拉，联动 KPI 卡 / 包明细 / 明细表）
  const [groups, setGroups] = useState<GroupView[]>([]);
  const [groupFilter, setGroupFilter] = useState<string[]>([]);
  const [acctFilter, setAcctFilter] = useState<string[]>([]);
  // Tab 布局（对齐国内 Dashboard：积分 / Token / 到期 / 请求明细）
  const [tab, setTab] = useState<'credits' | 'tokens' | 'expiry' | 'sessions'>('credits');
  // 数据源切换（对齐国内 Dashboard 第二层分类：本地 / API网关 / 官网，按 Tab 独立记忆）
  const [creditsSource, setCreditsSource] = useState<BoardSource>('local');
  const [tokensSource, setTokensSource] = useState<BoardSource>('local');
  // 官网聚合（31 天零填充；懒加载：积分统计 Tab 首次切到官网源才拉取）
  const [officialAll, setOfficialAll] = useState<BuddyAiUsageOfficialAll | null>(null);
  const officialLoadedRef = useRef(false);
  // 本地 Token 统计（~/.workbuddy-ai/projects 会话扫描；懒加载：Token Tab 首次激活才拉取）
  const [tokenStats, setTokenStats] = useState<BuddyAiTokenStats | null>(null);
  const tokensLoadedRef = useRef(false);
  // 载入/同步双守卫（审查 #22 + 复审 No.1）：loadSeqRef 单调分配请求序号；
  // dataSeqRef 为数据写权版本——仅 seq 更大的请求允许写数据并推进版本，失败不占号
  // （失败的同步不剥夺被其取代的在途加载的写权，避免有效新快照被误弃成误导空态）
  const loadSeqRef = useRef(0);
  const dataSeqRef = useRef(0);

  const load = useCallback(async () => {
    const seq = ++loadSeqRef.current;
    setLoading(true);
    try {
      // 主数据 accountsList 失败仅局部报错，不丢弃其余四路已成功的数据（对齐各路独立容错）
      const [accs, recs, usg, hist, gs] = await Promise.all([
        api.buddyai.accountsList().catch((err) => {
          pushToast('error', `账号列表读取失败：${String(err).slice(0, 120)}`);
          return null as BuddyAiAccountView[] | null;
        }),
        api.buddyai.checkinResults(90).catch(() => [] as WbCheckinRecord[]),
        api.apiServer.buddyAiUsageStats(90).catch(() => [] as UsageDayView[]),
        api.buddyai.creditsHistory(90).catch(() => [] as CreditSnap[]),
        api.groups.sharedList().catch(() => [] as GroupView[]),
      ]);
      // 写权版本门控（复审 No.1）：仅比已落盘版本更新的请求允许写数据并推进版本
      if (seq > dataSeqRef.current) {
        dataSeqRef.current = seq;
        if (accs) setAccounts(accs);
        setRecords(recs);
        setGroups(gs);
        setUsage(usg);
        setHistory(hist);
      }
    } finally {
      // 无条件复位加载态：本请求只复位自己发起的 loading（取代它的请求自行管理）
      setLoading(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // 官网聚合拉取（后端 10min 缓存 + stale-on-error；「刷新数据」联动重拉）
  const loadOfficial = useCallback(async () => {
    try {
      setOfficialAll(await api.buddyai.usageOfficialAll());
    } catch (err) {
      pushToast('error', `官网用量聚合查询失败：${String(err).slice(0, 160)}`);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  // 本地 Token 统计（后端 10min 结果缓存；fresh=true 强制重扫）
  const loadTokenStats = useCallback(async (fresh: boolean) => {
    try {
      setTokenStats(await withMinDelay(api.buddyai.tokenStats(fresh), 600));
    } catch (err) {
      pushToast('error', `本地 Token 统计失败：${String(err).slice(0, 160)}`);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  // 懒加载（对齐国内 §3.4）：官网聚合在积分统计 Tab 官网源首次激活时拉取；
  // 本地 Token 在 Token Tab 本地源首次激活时拉取
  useEffect(() => {
    if (tab === 'credits' && creditsSource === 'official' && !officialLoadedRef.current) {
      officialLoadedRef.current = true;
      void loadOfficial();
    }
    if (tab === 'tokens' && tokensSource === 'local' && !tokensLoadedRef.current) {
      tokensLoadedRef.current = true;
      void loadTokenStats(false);
    }
  }, [tab, creditsSource, tokensSource, loadOfficial, loadTokenStats]);

  const syncCredits = async () => {
    // 与 load 共享双守卫（审查 #22 + 复审 No.1）：请求序号单调分配；成功写数据须通过
    // 写权版本门控并推进版本，失败仅报错不占号（不剥夺被其取代的在途 load 的写权）
    const seq = ++loadSeqRef.current;
    setSyncing(true);
    try {
      const n = await withMinDelay(api.buddyai.creditsFetch(), 800);
      if (seq > dataSeqRef.current) {
        dataSeqRef.current = seq;
        setAccounts(n);
        const ok = n.filter((a) => a.credits_balance != null).length;
        pushToast('success', `积分已同步（${ok}/${n.length} 个账号取数成功）`);
        // 已加载过的源随全局刷新联动（官网聚合 10min 缓存过期后此路强制重拉窗口内数据）
        if (officialLoadedRef.current) void loadOfficial();
        if (tokensLoadedRef.current) void loadTokenStats(true);
        // 会话明细落地缓存重查（积分同步顺带官网聚合 → 行级明细随路落盘）
        setSessReload((k) => k + 1);
      }
    } catch (err) {
      // 失败不占号：无条件报错（不写数据、不推进写权版本）
      pushToast('error', `积分同步失败：${String(err).slice(0, 160)}`);
    } finally {
      // 无条件复位同步态；loading 由发起它的 load 自行复位（原「补位清 loading」不再需要）
      setSyncing(false);
    }
  };

  const list = accounts ?? [];
  // 全局筛选（国内看板同款多选语义：分组 → 账号级联，联动下方 KPI / 包明细 / 明细表；
  // 生效集 = 组筛选 ∩ 账号筛选，均空 = 不限）
  const groupFiltered = useMemo(() => {
    if (groupFilter.length === 0) return list;
    return list.filter(
      (a) =>
        (groupFilter.includes('__ungrouped__') && !a.group_id) ||
        (a.group_id != null && groupFilter.includes(a.group_id)),
    );
  }, [list, groupFilter]);
  const listF = useMemo(
    () => (acctFilter.length === 0 ? groupFiltered : groupFiltered.filter((a) => acctFilter.includes(a.id))),
    [groupFiltered, acctFilter],
  );
  const withBalance = listF.filter((a) => a.credits_balance != null);
  const total = withBalance.reduce((s, a) => s + (a.credits_balance ?? 0), 0);
  const nowSec = Math.floor(Date.now() / 1000);
  const neverFetched = listF.length - withBalance.length;
  const localDateStr = (dt: Date) =>
    `${dt.getFullYear()}-${String(dt.getMonth() + 1).padStart(2, '0')}-${String(dt.getDate()).padStart(2, '0')}`;
  const today = localDateStr(new Date());
  // 打卡获得（P4 同口径：今日）
  const todayEarned = useMemo(
    () =>
      records
        .filter((r) => r.date === today && r.status === 'success')
        // 随分组/账号筛选联动（对齐国内「筛选作用于 KPI」口径）
        .filter((r) => listF.some((a) => a.id === r.user_id))
        .reduce((s, r) => s + (r.reward ?? 0), 0),
    [records, today, listF],
  );
  // 7 日内积分到期账号数（credits_expire_at 落在未来 7 天窗口）
  const expiringSoon = withBalance.filter((a) => {
    const e = a.credits_expire_at;
    return e != null && e > nowSec && e <= nowSec + 7 * 86400;
  }).length;
  // 模型消耗排行（积分口径，对齐国内；Top8 + 其余合计由渲染层切分）
  const modelRank = useMemo(() => {
    const m = new Map<string, { requests: number; credits: number }>();
    for (const d of usage) {
      for (const x of d.models ?? []) {
        const e = m.get(x.name) ?? { requests: 0, credits: 0 };
        e.requests += x.requests;
        e.credits += x.credits ?? 0;
        m.set(x.name, e);
      }
    }
    return [...m.entries()]
      .map(([name, v]) => ({ name, ...v }))
      .sort((a, b) => b.credits - a.credits);
  }, [usage]);
  const rankMax = modelRank[0]?.credits ?? 0;
  // 积分包明细聚合（P9：各账号 credits_packages 拍平；按到期升序——对齐国内「积分到期」Tab 逐包到期口径，7 日内到期徽标）
  const pkgList = useMemo(
    () =>
      listF
        .flatMap((a) =>
          (a.credits_packages ?? []).map((p) => ({
            acct: a.nickname || a.id,
            ...p,
          })),
        )
        .sort((x, y) => (x.expire_secs ?? Infinity) - (y.expire_secs ?? Infinity)),
    [listF],
  );
  const pkgSoon = (expire: number | null | undefined) =>
    expire != null && expire > nowSec && expire <= nowSec + 7 * 86400;
  // 积分到期 Tab（对齐国内 ExpiryTab 结构）：账号积分明细行 + 到期日历 items
  // 包计数与国内口径对齐：只计剩余 > 0 的包（已用完不计）
  const acctExpiryRows = useMemo(
    () =>
      listF.map((a) => {
        const pkgs = (a.credits_packages ?? []).filter((p) => p.remain > 0);
        const expires = pkgs
          .map((p) => p.expire_secs)
          .filter((t): t is number => t != null && t > 0);
        return {
          a,
          pkgCount: pkgs.length,
          nearest: expires.length > 0 ? Math.min(...expires) : a.credits_expire_at,
        };
      }),
    [listF],
  );
  const expiryItems = useMemo<ExpiryItem[]>(
    () =>
      listF.flatMap((a) =>
        (a.credits_packages ?? [])
          .filter((p) => p.remain > 0)
          .map((p) => ({
            key: `buddyai-${a.id}-${p.code || p.name}-${p.expire_secs ?? 0}`,
            label: `BuddyAI · ${a.nickname || a.id} · ${p.name || p.code || '积分包'}`,
            kind: '积分包',
            expire_ts: p.expire_secs ?? null,
            note: `剩余 ${p.remain.toFixed(2)} / 总 ${p.size.toFixed(0)}`,
          })),
      ),
    [listF],
  );
  // 今日消耗积分（网关记账口径：usage 今日 credits 合计——对齐国内「今日消耗积分」）
  const todayCreditsSpent = useMemo(() => {
    const d = usage.find((u) => u.date === today);
    return d ? (d.models ?? []).reduce((s, x) => s + (x.credits ?? 0), 0) : 0;
  }, [usage, today]);

  // ── 积分统计区间（对齐国内 CreditsTab：今日/近7天/近30天/本月/近一年）──
  // 数据窗口：打卡记录与网关用量后端均 clamp 90 天。近一年档的起点钳到 90 天
  // 窗口（审查 P2：不钳会生成 365 个数据点、前 275 天恒 0，视觉误导为「半年
  // 没消耗」；钳后区间统计/趋势均只覆盖可得数据，卡片下方有窗口声明）
  const [range, setRange] = useState<'today' | '7d' | '30d' | 'month' | 'year'>('30d');
  const rangeStart = useMemo(() => {
    if (range === 'today') return today;
    const d = new Date();
    if (range === '7d') d.setDate(d.getDate() - 6);
    else if (range === '30d') d.setDate(d.getDate() - 29);
    else if (range === 'month') d.setDate(1);
    else d.setDate(d.getDate() - 89);
    return localDateStr(d);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [range, today]);

  // 按日序列：获得 = 打卡入账（success 计奖）；消耗 = 网关记账 credits
  const dailyEarned = useMemo(() => {
    const m = new Map<string, number>();
    for (const r of records) {
      if (r.status === 'success' && r.reward) m.set(r.date, (m.get(r.date) ?? 0) + r.reward);
    }
    return m;
  }, [records]);
  const dailySpent = useMemo(() => {
    const m = new Map<string, number>();
    for (const d of usage) {
      const c = (d.models ?? []).reduce((s, x) => s + (x.credits ?? 0), 0);
      if (c > 0) m.set(d.date, (m.get(d.date) ?? 0) + c);
    }
    return m;
  }, [usage]);

  // ── 本地源序列（对齐国内 Buddy：快照 earned 获得 + 逐账号余额差分消耗）──
  // 账号筛选生效时快照行 accounts 同步收窄（earned 无账号维度，过滤态回退打卡口径）
  const filteredHistory = useMemo(() => {
    if (acctFilter.length === 0 && groupFilter.length === 0) return history;
    const ids = new Set(listF.map((a) => a.id));
    return history.map((s) => ({ ...s, accounts: s.accounts.filter((x) => ids.has(x.id)) }));
  }, [history, acctFilter, groupFilter, listF]);
  const hasSnapEarned = useMemo(() => filteredHistory.some((s) => s.earned != null), [filteredHistory]);
  const localEarned = useMemo(() => {
    // 无筛选且快照含 earned → 快照口径；否则回退打卡 reward 按当前筛选账号过滤
    if (hasSnapEarned && acctFilter.length === 0 && groupFilter.length === 0) {
      const m = new Map<string, number>();
      for (const s of filteredHistory) {
        if (s.earned != null && s.earned > 0) m.set(s.date, (m.get(s.date) ?? 0) + s.earned);
      }
      return m;
    }
    const m = new Map<string, number>();
    const ids = new Set(listF.map((a) => a.id));
    for (const r of records) {
      if (r.status === 'success' && r.reward && ids.has(r.user_id)) {
        m.set(r.date, (m.get(r.date) ?? 0) + r.reward);
      }
    }
    return m;
  }, [filteredHistory, hasSnapEarned, acctFilter, groupFilter, listF, records]);
  const localSpent = useMemo(() => {
    // 逐账号余额日差分（负差 = 消耗）；首日无前值不计（对齐国内快照差分口径）
    const m = new Map<string, number>();
    const prev = new Map<string, number>();
    const sorted = [...filteredHistory].sort((a, b) => a.date.localeCompare(b.date));
    for (const s of sorted) {
      let spent = 0;
      for (const x of s.accounts) {
        const p = prev.get(x.id);
        if (p != null && x.balance != null && p > x.balance) spent += p - x.balance;
        if (x.balance != null) prev.set(x.id, x.balance);
      }
      if (spent > 0) m.set(s.date, (m.get(s.date) ?? 0) + spent);
    }
    return m;
  }, [filteredHistory]);
  // 官网源序列（31 天零填充聚合）
  const officialSpent = useMemo(() => {
    const m = new Map<string, number>();
    for (const d of officialAll?.daily ?? []) if (d.usage > 0) m.set(d.date, d.usage);
    return m;
  }, [officialAll]);

  // 按源取数（网关源无获得数据，避免净消耗系统性偏负——对齐国内 §3.1 口径决策）
  const srcEarned = creditsSource === 'gateway' ? EMPTY_MAP : localEarned;
  const srcSpent =
    creditsSource === 'gateway' ? dailySpent : creditsSource === 'official' ? officialSpent : localSpent;

  // 区间三统计卡（获得 − 消耗 = 净获得）
  const rangeEarned = useMemo(() => {
    let s = 0;
    for (const [k, v] of srcEarned) if (k >= rangeStart) s += v;
    return s;
  }, [srcEarned, rangeStart]);
  const rangeSpent = useMemo(() => {
    let s = 0;
    for (const [k, v] of srcSpent) if (k >= rangeStart) s += v;
    return s;
  }, [srcSpent, rangeStart]);
  const rangeNet = rangeEarned - rangeSpent;

  // 网关源请求统计（区间合计）
  const rangeUsage = useMemo(() => usage.filter((u) => u.date >= rangeStart), [usage, rangeStart]);
  const gwRequests = rangeUsage.reduce((s, u) => s + u.total_requests, 0);
  const gwOk = rangeUsage.reduce((s, u) => s + u.ok, 0);
  const gwErrors = rangeUsage.reduce((s, u) => s + u.errors, 0);

  // 趋势序列（区间内逐日；无数据日补 0 保持连续横轴）
  const trendData = useMemo(() => {
    const out: Array<{ date: string; 获得积分: number | null; 消耗积分: number }> = [];
    const end = new Date();
    const start = new Date(`${rangeStart}T00:00:00`);
    for (let d = new Date(start); d <= end; d.setDate(d.getDate() + 1)) {
      const k = localDateStr(d);
      out.push({
        date: k.slice(5),
        获得积分: creditsSource === 'gateway' ? null : Number((srcEarned.get(k) ?? 0).toFixed(2)),
        消耗积分: Number((srcSpent.get(k) ?? 0).toFixed(2)),
      });
    }
    return out;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rangeStart, srcEarned, srcSpent, creditsSource]);

  // 年度活动热力图（消耗口径，随数据源切换；窗口诚实标注）
  const heatValues = useMemo(() => {
    const m = new Map<string, number>();
    for (const [k, v] of srcSpent) if (v > 0) m.set(k, v);
    return m;
  }, [srcSpent]);
  // 模型消耗排行：网关 = 按积分（近 90 天）；官网 = 31 天全窗口汇总；本地无模型明细
  const officialModelRank = useMemo(() => {
    const models = officialAll?.models;
    if (!models) return [];
    return models.map((m) => ({ name: m.model, requests: m.request_count, credits: m.credit }));
  }, [officialAll]);

  // 会话明细查询（纯缓存零网络；筛选变更自动查询，对齐国内 SessionsTab）
  useEffect(() => {
    if (sessStart > sessEnd) {
      setSessData(null);
      return;
    }
    let cancelled = false;
    setSessLoading(true);
    api.buddyai
      .usageSessions({
        start: sessStart,
        end: sessEnd,
        model: sessModel || null,
        userId: sessUser || null,
        page: sessPage,
        pageSize: 50,
      })
      .then((r) => {
        if (cancelled) return;
        setSessData(r);
        setSessModels((prev) => Array.from(new Set([...prev, ...(r.models ?? [])])).sort());
        // 空页回夹：以服务端夹取后的页码为准回同步
        if (r.page !== sessPage) setSessPage(r.page);
      })
      .catch((err) => {
        if (cancelled) return;
        pushToast('error', `会话明细查询失败：${String(err).slice(0, 160)}`);
        setSessData(null);
      })
      .finally(() => {
        if (!cancelled) setSessLoading(false);
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sessStart, sessEnd, sessModel, sessUser, sessPage, sessReload]);
  // ── Token 统计切面（对齐国内 TokensTab：独立区间 + 四卡 + 柱线组合图）──
  const [tokenRange, setTokenRange] = useState<'today' | '7d' | '30d' | 'month' | 'year'>('30d');
  const tokenRangeStart = useMemo(() => {
    if (tokenRange === 'today') return today;
    const d = new Date();
    if (tokenRange === '7d') d.setDate(d.getDate() - 6);
    else if (tokenRange === '30d') d.setDate(d.getDate() - 29);
    else if (tokenRange === 'month') d.setDate(1);
    else d.setDate(d.getDate() - 89);
    return localDateStr(d);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tokenRange, today]);
  const usageInRange = useMemo(
    () => usage.filter((u) => u.date >= tokenRangeStart),
    [usage, tokenRangeStart],
  );
  const sumTokens = useMemo(
    () => usageInRange.reduce((s, u) => s + u.prompt_tokens + u.completion_tokens, 0),
    [usageInRange],
  );
  const sumPrompt = useMemo(() => usageInRange.reduce((s, u) => s + u.prompt_tokens, 0), [usageInRange]);
  const sumCompletion = useMemo(
    () => usageInRange.reduce((s, u) => s + u.completion_tokens, 0),
    [usageInRange],
  );
  const sumRequests = useMemo(() => usageInRange.reduce((s, u) => s + u.total_requests, 0), [usageInRange]);
  const sumErrors = useMemo(() => usageInRange.reduce((s, u) => s + u.errors, 0), [usageInRange]);
  // 柱线组合趋势（输入/输出堆叠柱 + 调用次数虚线，对齐国内网关源口径）
  const tokenTrend = useMemo(
    () =>
      usageInRange.map((u) => ({
        date: u.date.slice(5),
        输入: u.prompt_tokens,
        输出: u.completion_tokens,
        调用: u.total_requests,
      })),
    [usageInRange],
  );
  // Token 热力图（全量 90 天窗口 tokens，不受区间筛选影响——对齐国内热力图口径）
  const tokenHeat = useMemo(() => {
    const m = new Map<string, number>();
    for (const u of usage) {
      const v = u.prompt_tokens + u.completion_tokens;
      if (v > 0) m.set(u.date, v);
    }
    return m;
  }, [usage]);
  // Token 模型排行（网关源无按模型 token 明细 → 按请求数口径，对齐国内网关源诚实声明）
  const tokenModelRank = useMemo(() => {
    const m = new Map<string, number>();
    for (const d of usageInRange) {
      for (const x of d.models ?? []) m.set(x.name, (m.get(x.name) ?? 0) + x.requests);
    }
    return [...m.entries()].map(([name, requests]) => ({ name, requests })).sort((a, b) => b.requests - a.requests);
  }, [usageInRange]);
  const tokenRankMax = tokenModelRank[0]?.requests ?? 0;
  // Token 本地源派生（~/.workbuddy-ai 会话扫描；区间聚合 + 模型排行 + 热力图）
  const tokenLocal = useMemo(() => {
    if (!tokenStats) return null;
    const byDate = new Map<string, { input: number; output: number; cache_read: number; cache_write: number; calls: number }>();
    const byModel = new Map<string, { tokens: number; calls: number }>();
    let input = 0, output = 0, cacheRead = 0, cacheWrite = 0, calls = 0;
    for (const [model, points] of Object.entries(tokenStats.daily_by_model)) {
      for (const p of points) {
        if (!p.date || p.date < tokenRangeStart) continue;
        const i = p.input ?? 0, o = p.output ?? 0, cr = p.cache_read ?? 0, cw = p.cache_write ?? 0, c = p.calls ?? 0;
        input += i; output += o; cacheRead += cr; cacheWrite += cw; calls += c;
        const d = byDate.get(p.date) ?? { input: 0, output: 0, cache_read: 0, cache_write: 0, calls: 0 };
        d.input += i; d.output += o; d.cache_read += cr; d.cache_write += cw; d.calls += c;
        byDate.set(p.date, d);
        const e = byModel.get(model) ?? { tokens: 0, calls: 0 };
        e.tokens += i + o + cw;
        e.calls += c;
        byModel.set(model, e);
      }
    }
    return { input, output, cacheRead, cacheWrite, calls, byDate, byModel };
  }, [tokenStats, tokenRangeStart]);
  const tokenLocalTrend = useMemo(() => {
    const out: Array<Record<string, number | string>> = [];
    const end = new Date();
    const start = new Date(`${tokenRangeStart}T00:00:00`);
    for (let d = new Date(start); d <= end; d.setDate(d.getDate() + 1)) {
      const k = localDateStr(d);
      const a = tokenLocal?.byDate.get(k);
      out.push({
        date: k.slice(5),
        缓存读取: a?.cache_read ?? 0,
        新增输入: Math.max(0, (a?.input ?? 0) - (a?.cache_read ?? 0)),
        输出: a?.output ?? 0,
        缓存写入: a?.cache_write ?? 0,
        调用: a?.calls ?? 0,
      });
    }
    return out;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tokenLocal, tokenRangeStart]);
  const tokenLocalRank = useMemo(() => {
    if (!tokenLocal) return [];
    return [...tokenLocal.byModel.entries()]
      .map(([name, m]) => ({ name, tokens: m.tokens }))
      .sort((a, b) => b.tokens - a.tokens);
  }, [tokenLocal]);
  const tokenLocalRankMax = tokenLocalRank[0]?.tokens ?? 0;
  const tokenLocalHeat = useMemo(() => {
    const m = new Map<string, number>();
    for (const p of tokenStats?.daily ?? []) {
      if (p.date && p.total > 0) m.set(p.date, p.total);
    }
    return m;
  }, [tokenStats]);

  const fmtExpire = (ts: number | null) =>
    ts == null ? '—' : new Date(ts * 1000).toLocaleString();
  const fmtFetched = (iso: string | null) =>
    iso == null || iso === '' ? '—' : new Date(iso).toLocaleString();

  return (
    <div className="animate-fade-in">
      <PageHeader
        title="BuddyAI · 积分看板"
        desc="积分余额 · 积分明细 · Token 统计 · 到期日历 · 会话明细"
      />

      {/* 顶部统计卡（P5 口径对齐国内积分看板七卡；随筛选联动） */}
      <div className="mt-4 grid grid-cols-2 gap-3 md:grid-cols-4 xl:grid-cols-7">
        <StatCard label="账号总数" value={listF.length} hint={`${withBalance.length} 个已取数`} tone="brand" />
        <StatCard
          label="可用总积分"
          value={withBalance.length ? total.toFixed(2) : '—'}
          hint={neverFetched > 0 ? `${neverFetched} 个账号未取数` : '全部账号已取数'}
          tone="amber"
        />
        <StatCard
          label="平均可用积分"
          value={withBalance.length ? (total / withBalance.length).toFixed(0) : '—'}
          hint="按已取数账号平均"
          tone="green"
        />
        <StatCard label="积分包总数" value={pkgList.length} hint={`${new Set(pkgList.map((p) => p.acct)).size} 个账号持有积分包`} tone="brand" />
        <StatCard label="今日新增积分" value={todayEarned > 0 ? todayEarned.toFixed(0) : '0'} hint="今日打卡入账合计（服务端按套餐 30/50 定档）" tone="green" />
        <StatCard label="今日消耗积分" value={todayCreditsSpent > 0 ? todayCreditsSpent.toFixed(2) : '0'} hint="今日网关用量记账积分合计" tone="amber" />
        <StatCard label="7 日内到期" value={expiringSoon} hint="积分包到期账号数" tone={expiringSoon > 0 ? 'amber' : 'slate'} />
      </div>

      {/* 第一层分类：Tab 切换（左，胶囊分组样式对齐国内 Dashboard）+ 筛选/刷新（右） */}
      <div className="mb-3 mt-6 flex flex-wrap items-center justify-between gap-2">
        <div className="flex rounded-lg bg-slate-100 p-1 dark:bg-zinc-900">
          {(
            [
              ['credits', '积分统计'],
              ['tokens', 'Token 统计'],
              ['expiry', '积分到期'],
              ['sessions', '会话明细'],
            ] as const
          ).map(([k, label]) => (
            <button
              key={k}
              className={`rounded-md px-3.5 py-1.5 text-xs font-medium ${
                tab === k
                  ? 'bg-white text-zinc-900 shadow-sm dark:bg-zinc-800 dark:text-zinc-100'
                  : 'text-slate-500 dark:text-zinc-400'
              }`}
              onClick={() => setTab(k)}
            >
              {label}
            </button>
          ))}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {/* 分组/账号多选下拉（对齐国内 Dashboard MultiSelectDropdown：chips 触发 + 面板勾选；
              账号候选随分组筛选联动） */}
          <MultiSelectDropdown
            label="分组筛选"
            options={[
              { id: '__ungrouped__', name: '未分组' },
              ...groups.map((g) => ({ id: g.id, name: g.name, color: g.color })),
            ]}
            selected={groupFilter}
            onChange={setGroupFilter}
          />
          <MultiSelectDropdown
            label="账号筛选"
            options={groupFiltered.map((a) => ({ id: a.id, name: a.nickname || a.id }))}
            selected={acctFilter}
            onChange={setAcctFilter}
          />
          {acctFilter.length > 0 || groupFilter.length > 0 ? (
            <button
              className="btn-ghost text-xs text-slate-400"
              onClick={() => {
                setGroupFilter([]);
                setAcctFilter([]);
              }}
            >
              清除筛选
            </button>
          ) : null}
          <span className="text-[11px] text-slate-400">
            {listF.length}/{list.length} 个账号
          </span>
          <button className="btn-outline inline-flex items-center gap-1.5 text-xs" onClick={() => void syncCredits()} disabled={syncing}>
            <RefreshCw size={13} className={syncing ? 'animate-spin' : ''} /> {syncing ? '刷新中…' : '刷新数据'}
          </button>
        </div>
      </div>

      {/* 积分统计（对齐国内 CreditsTab：数据源可切换 + 按源统计卡 + 趋势 + 时间筛选平铺） */}
      {tab === 'credits' && (
      <div className="card mt-4 p-4">
        <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-sm font-medium">积分统计</span>
            {/* 数据源切换（对齐国内 Dashboard 第二层分类：本地 / API网关 / 官网） */}
            <span className="text-[11px] text-slate-400">数据源</span>
            <div className="flex rounded-lg bg-slate-100 p-1 dark:bg-zinc-900">
              {SOURCES.map((s) => {
                const tip =
                  s === 'local'
                    ? '本地积分快照（buddy_ai_credits_history，每日同步落库；消耗为逐账号余额差分）'
                    : s === 'gateway'
                      ? 'API 网关用量记账（buddy_ai_days，模型倍率 × 用量，90 天窗口）'
                      : '官网请求流水聚合（get-user-request-usage 全账号 31 天，10 分钟缓存）';
                return (
                  <button
                    key={s}
                    title={tip}
                    className={`rounded-md px-3 py-1 text-xs font-medium transition ${
                      creditsSource === s
                        ? 'bg-white text-zinc-900 shadow-sm dark:bg-zinc-800 dark:text-zinc-100'
                        : 'text-slate-500 dark:text-zinc-400'
                    }`}
                    onClick={() => setCreditsSource(s)}
                  >
                    {SOURCE_LABELS[s]}
                  </button>
                );
              })}
            </div>
            <span className="text-[11px] text-slate-400">
              {creditsSource === 'local'
                ? '快照 earned 获得 · 余额差分消耗'
                : creditsSource === 'gateway'
                  ? '记账口径 · 90 天窗口'
                  : officialAll?.stale
                    ? '缓存回退 · 非当日数据'
                    : '官网流水 · 31 天窗口'}
            </span>
          </div>
          {/* 时间筛选平铺（对齐国内：胶囊单选，不使用下拉） */}
          <div className="flex overflow-hidden rounded-lg border border-slate-200 text-xs dark:border-zinc-700">
            {(
              [
                ['today', '今日'],
                ['7d', '近7天'],
                ['30d', '近30天'],
                ['month', '本月'],
                ['year', '近一年'],
              ] as const
            ).map(([k, label]) => (
              <button
                key={k}
                className={`px-2.5 py-1 transition ${
                  range === k
                    ? 'bg-brand-500/10 font-medium text-brand-600 dark:text-brand-400'
                    : 'text-slate-500 hover:bg-slate-50 dark:text-zinc-400 dark:hover:bg-zinc-800/60'
                }`}
                onClick={() => setRange(k)}
              >
                {label}
              </button>
            ))}
          </div>
        </div>

        {/* 区间统计卡（网关源 = 消耗/成功/失败；本地/官网源 = 获得/消耗/净获得） */}
        {creditsSource === 'gateway' ? (
          <div className="mb-3 grid grid-cols-1 gap-3 sm:grid-cols-3">
            <div className="rounded-lg border-t-2 border-amber-500 bg-slate-50/60 p-3 dark:bg-zinc-900/60">
              <div className="text-xs text-slate-500">区间总消耗</div>
              <div className="text-xl font-bold tabular-nums text-amber-500">{rangeSpent.toFixed(2)}</div>
              <div className="text-[11px] text-slate-400">网关用量记账（模型倍率 × 用量）</div>
            </div>
            <div className="rounded-lg border-t-2 border-emerald-500 bg-slate-50/60 p-3 dark:bg-zinc-900/60">
              <div className="text-xs text-slate-500">成功请求</div>
              <div className="text-xl font-bold tabular-nums text-emerald-500">{gwOk.toLocaleString()}</div>
              <div className="text-[11px] text-slate-400">区间内网关转发成功请求合计</div>
            </div>
            <div className="rounded-lg border-t-2 border-rose-500 bg-slate-50/60 p-3 dark:bg-zinc-900/60">
              <div className="text-xs text-slate-500">失败请求</div>
              <div className="text-xl font-bold tabular-nums text-rose-500">{gwErrors.toLocaleString()}</div>
              <div className="text-[11px] text-slate-400">超时/上游错误计入</div>
            </div>
          </div>
        ) : (
          <div className="mb-3 grid grid-cols-1 gap-3 sm:grid-cols-3">
            <div className="rounded-lg border-t-2 border-emerald-500 bg-slate-50/60 p-3 dark:bg-zinc-900/60">
              <div className="text-xs text-slate-500">区间总获得</div>
              <div className="text-xl font-bold tabular-nums text-emerald-500">{rangeEarned.toFixed(2)}</div>
              <div className="text-[11px] text-slate-400">
                {creditsSource === 'local' ? '快照 earned（缺失日回退打卡口径）' : '积分快照 earned 口径'}
              </div>
            </div>
            <div className="rounded-lg border-t-2 border-amber-500 bg-slate-50/60 p-3 dark:bg-zinc-900/60">
              <div className="text-xs text-slate-500">区间总消耗</div>
              <div className="text-xl font-bold tabular-nums text-amber-500">{rangeSpent.toFixed(2)}</div>
              <div className="text-[11px] text-slate-400">
                {creditsSource === 'local' ? '逐账号余额日差分推导' : '官网请求流水 credit 合计'}
              </div>
            </div>
            <div className="rounded-lg border-t-2 border-rose-500 bg-slate-50/60 p-3 dark:bg-zinc-900/60">
              <div className="text-xs text-slate-500">净获得</div>
              <div className={`text-xl font-bold tabular-nums ${rangeNet >= 0 ? 'text-emerald-500' : 'text-rose-500'}`}>
                {rangeNet >= 0 ? '+' : ''}{rangeNet.toFixed(2)}
              </div>
              <div className="text-[11px] text-slate-400">获得 − 消耗</div>
            </div>
          </div>
        )}

        {(range === 'year' || creditsSource === 'official') && (
          <p className="mb-3 text-[11px] text-slate-400">
            {creditsSource === 'official'
              ? '注：官网源仅覆盖近 31 天窗口（全账号聚合，跨账号合并）；区间超出部分按可得数据统计。'
              : '注：打卡记录与网关用量仅保留近 90 天窗口，「近一年」区间按可得数据统计（起点自动钳到窗口内）。'}
          </p>
        )}

        {creditsSource === 'local' && !hasSnapEarned && localSpent.size === 0 ? (
          <p className="py-6 text-center text-xs text-slate-400">
            暂无本地快照数据。打开「积分看板」或点「刷新数据」完成积分同步后按日落库，即可看到趋势。
          </p>
        ) : trendData.every((d) => d.消耗积分 === 0 && (d.获得积分 == null || d.获得积分 === 0)) ? (
          <p className="py-6 text-center text-xs text-slate-400">
            {creditsSource === 'gateway'
              ? '该区间无网关消耗记录（启用 buddy-ai 国际档并通过网关调用后展示）。'
              : creditsSource === 'official'
                ? '该区间无官网消耗记录（官网源覆盖近 31 天；点「刷新数据」重拉后再试）。'
                : '该区间无本地快照消耗记录。'}
          </p>
        ) : (
          <div className="h-56">
            <ResponsiveContainer width="100%" height="100%">
              <LineChart data={trendData} margin={{ top: 4, right: 8, bottom: 0, left: -12 }}>
                <CartesianGrid strokeDasharray="3 3" stroke="#8884" vertical={false} />
                <XAxis dataKey="date" tick={{ fontSize: 10 }} interval="preserveStartEnd" tickLine={false} axisLine={false} />
                <YAxis tick={{ fontSize: 10 }} tickLine={false} axisLine={false} />
                <Tooltip contentStyle={{ fontSize: 12, borderRadius: 8 }} />
                <Legend wrapperStyle={{ fontSize: 12 }} />
                {creditsSource !== 'gateway' && (
                  <Line type="monotone" dataKey="获得积分" stroke="#10b981" strokeWidth={2} dot={false} connectNulls />
                )}
                <Line
                  type="monotone"
                  dataKey="消耗积分"
                  name={creditsSource === 'gateway' ? '消耗积分（网关记账）' : '消耗积分'}
                  stroke="#f59e0b"
                  strokeWidth={2}
                  dot={false}
                />
              </LineChart>
            </ResponsiveContainer>
          </div>
        )}
      </div>
      )}

      {/* 积分到期（对齐国内 ExpiryTab：账号积分明细 + 积分到期日历 + 积分包明细） */}
      {tab === 'expiry' && (
      <>
        {/* ① 账号积分明细（含积分包个数 / 最近到期，对齐国内 ExpiryTab 表结构） */}
        <div className="card mt-4 overflow-x-auto p-4">
          <div className="mb-3 flex items-center justify-between">
            <span className="text-sm font-medium">账号积分明细</span>
            <span className="text-[11px] text-slate-400">积分包个数只计剩余 &gt; 0 的包（刷新数据后更新）</span>
          </div>
          {acctExpiryRows.length === 0 ? (
            <p className="py-4 text-center text-xs text-slate-400">当前筛选条件下无账号。</p>
          ) : (
            <table className="w-full min-w-[680px] text-sm">
              <thead>
                <tr className="border-b border-slate-100 text-left text-xs text-slate-400 dark:border-zinc-800">
                  <th className="py-2 pr-3 font-medium">账号</th>
                  <th className="py-2 pr-3 font-medium">可用积分</th>
                  <th className="py-2 pr-3 font-medium" title="剩余 > 0 的积分包计数（与顶部「积分包总数」口径一致）">
                    积分包
                  </th>
                  <th className="py-2 pr-3 font-medium">最近到期</th>
                  <th className="py-2 pr-3 font-medium">取数状态</th>
                </tr>
              </thead>
              <tbody>
                {acctExpiryRows.map(({ a, pkgCount, nearest }) => (
                  <tr key={a.id} className="border-b border-slate-50 last:border-0 dark:border-zinc-800/50">
                    <td className="py-2.5 pr-3">
                      <div className="font-medium">{a.nickname || a.id}</div>
                      <div className="text-[11px] text-slate-400">{a.uid || a.id}</div>
                    </td>
                    <td className="py-2.5 pr-3 tabular-nums">
                      {a.credits_balance != null ? (
                        <span className="inline-flex items-center gap-1">
                          <Coins size={13} className="text-amber-500" />
                          {a.credits_balance.toFixed(2)}
                        </span>
                      ) : (
                        <span className="text-xs text-slate-400">未取数</span>
                      )}
                    </td>
                    <td className="py-2.5 pr-3 text-xs tabular-nums text-slate-500">{pkgCount} 个</td>
                    <td className="py-2.5 pr-3 text-xs tabular-nums">
                      {nearest != null ? (
                        <span className={nearest <= nowSec ? 'text-rose-500' : pkgSoon(nearest) ? 'text-amber-500' : 'text-slate-500'}>
                          {new Date(nearest * 1000).toLocaleDateString()}
                          {nearest <= nowSec ? '（已过期）' : pkgSoon(nearest) ? '（7 日内）' : ''}
                        </span>
                      ) : (
                        <span className="text-slate-400">—</span>
                      )}
                    </td>
                    <td className="py-2.5 pr-3 text-xs text-slate-500">{fmtFetched(a.credits_fetched_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>

        {/* ② 积分到期日历（复用国内 ExpiryCalendar：按月分组倒序 + 倒计时徽标） */}
        <div className="card mt-4 p-4">
          <h3 className="mb-3 text-sm font-medium">积分到期日历</h3>
          <ExpiryCalendar
            items={expiryItems}
            emptyHint="暂无到期项：先「刷新数据」完成积分同步，取得积分包明细后展示。"
          />
        </div>
      </>
      )}

      {/* Token 统计（对齐国内 TokensTab：数据源切换 + 独立区间胶囊 + 四统计卡 + 柱线组合图） */}
      {tab === 'tokens' && (
      <div className="card mt-4 p-4">
        <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-sm font-medium">Token 统计</span>
            {/* 数据源切换（对齐国内 Dashboard 第二层分类；官网源 BuddyAI 无接口 → 禁用） */}
            <span className="text-[11px] text-slate-400">数据源</span>
            <div className="flex rounded-lg bg-slate-100 p-1 dark:bg-zinc-900">
              {SOURCES.map((s) => {
                const reason =
                  s === 'official' ? 'BuddyAI 官网未提供按日 token 明细接口' : undefined;
                return (
                  <button
                    key={s}
                    disabled={!!reason}
                    title={reason ?? SOURCE_LABELS[s]}
                    className={`rounded-md px-3 py-1 text-xs font-medium transition ${
                      tokensSource === s
                        ? 'bg-white text-zinc-900 shadow-sm dark:bg-zinc-800 dark:text-zinc-100'
                        : 'text-slate-500 dark:text-zinc-400'
                    } ${reason ? 'cursor-not-allowed opacity-40' : ''}`}
                    onClick={() => setTokensSource(s)}
                  >
                    {SOURCE_LABELS[s]}
                  </button>
                );
              })}
            </div>
            <span className="text-[11px] text-slate-400">
              {tokensSource === 'local'
                ? tokenStats
                  ? `~/.workbuddy-ai 会话扫描 · ${tokenStats.files_scanned} 个文件 · ${tokenStats.window_days} 天窗口`
                  : '~/.workbuddy-ai 会话扫描（加载中…）'
                : '网关记账（buddy-ai 档）· 仅保留 90 天窗口 · ~/.workbuddy-ai 独立记账'}
            </span>
          </div>
          {/* 区间胶囊（独立于积分统计区间，对齐国内 TokensTab 顶栏） */}
          <div className="flex overflow-hidden rounded-lg border border-slate-200 text-xs dark:border-zinc-700">
            {(
              [
                ['today', '今日'],
                ['7d', '近7天'],
                ['30d', '近30天'],
                ['month', '本月'],
                ['year', '近一年'],
              ] as const
            ).map(([k, label]) => (
              <button
                key={k}
                className={`px-2.5 py-1 transition ${
                  tokenRange === k
                    ? 'bg-brand-500/10 font-medium text-brand-600 dark:text-brand-400'
                    : 'text-slate-500 hover:bg-slate-50 dark:text-zinc-400 dark:hover:bg-zinc-800/60'
                }`}
                onClick={() => setTokenRange(k)}
              >
                {label}
              </button>
            ))}
          </div>
        </div>
        {/* 四统计卡（本地源：总/输入/输出/缓存命中率；网关源：总/输入/输出/请求数） */}
        {tokensSource === 'local' ? (
          <>
            <div className="mb-3 grid grid-cols-2 gap-3 md:grid-cols-4">
              <div className="rounded-lg border-t-2 border-violet-500 bg-slate-50/60 p-3 dark:bg-zinc-900/60">
                <div className="text-xs text-slate-500">总 Tokens</div>
                <div className="text-xl font-bold tabular-nums text-violet-500">
                  {((tokenLocal?.input ?? 0) + (tokenLocal?.output ?? 0) + (tokenLocal?.cacheWrite ?? 0)).toLocaleString()}
                </div>
                <div className="text-[11px] text-slate-400">输入 + 输出 + 缓存写入</div>
              </div>
              <div className="rounded-lg border-t-2 border-sky-500 bg-slate-50/60 p-3 dark:bg-zinc-900/60">
                <div className="text-xs text-slate-500">输入 Tokens</div>
                <div className="text-xl font-bold tabular-nums text-sky-500">{(tokenLocal?.input ?? 0).toLocaleString()}</div>
                <div className="text-[11px] text-slate-400">其中缓存读取 {(tokenLocal?.cacheRead ?? 0).toLocaleString()}</div>
              </div>
              <div className="rounded-lg border-t-2 border-emerald-500 bg-slate-50/60 p-3 dark:bg-zinc-900/60">
                <div className="text-xs text-slate-500">输出 Tokens</div>
                <div className="text-xl font-bold tabular-nums text-emerald-500">{(tokenLocal?.output ?? 0).toLocaleString()}</div>
                <div className="text-[11px] text-slate-400">completion 合计</div>
              </div>
              <div className="rounded-lg border-t-2 border-amber-500 bg-slate-50/60 p-3 dark:bg-zinc-900/60">
                <div className="text-xs text-slate-500">缓存命中率</div>
                <div className="text-xl font-bold tabular-nums text-amber-500">
                  {(tokenLocal?.input ?? 0) > 0
                    ? `${(((tokenLocal?.cacheRead ?? 0) / (tokenLocal?.input ?? 1)) * 100).toFixed(1)}%`
                    : '—'}
                </div>
                <div className="text-[11px] text-slate-400">口径：缓存读取 / 输入总量</div>
              </div>
            </div>
            {!tokenStats ? (
              <p className="py-4 text-center text-xs text-slate-400">本地会话扫描中…（首次加载稍候）</p>
            ) : tokenLocalTrend.every(
                (d) => !(d['缓存读取'] as number) && !(d['新增输入'] as number) && !(d['输出'] as number) && !(d['缓存写入'] as number),
              ) ? (
              <p className="py-4 text-center text-xs text-slate-400">
                暂无本地会话用量：通过 WorkBuddy AI 国际客户端产生会话后展示（~/.workbuddy-ai/projects）。
              </p>
            ) : (
              <div className="h-56">
                <ResponsiveContainer width="100%" height="100%">
                  <ComposedChart data={tokenLocalTrend} margin={{ top: 8, right: 8, bottom: 0, left: -8 }}>
                    <CartesianGrid strokeDasharray="3 3" stroke="#8884" vertical={false} />
                    <XAxis dataKey="date" tick={{ fontSize: 10 }} interval="preserveStartEnd" tickLine={false} axisLine={false} />
                    <YAxis
                      yAxisId="tokens"
                      tick={{ fontSize: 10 }}
                      tickLine={false}
                      axisLine={false}
                      tickFormatter={(v: number) => `${Math.round(v / 1000)}k`}
                    />
                    <YAxis yAxisId="calls" orientation="right" tick={{ fontSize: 10 }} tickLine={false} axisLine={false} />
                    <Tooltip contentStyle={{ fontSize: 12, borderRadius: 8 }} />
                    <Legend wrapperStyle={{ fontSize: 12 }} />
                    <Bar yAxisId="tokens" dataKey="缓存读取" stackId="t" fill="#6366f1" />
                    <Bar yAxisId="tokens" dataKey="新增输入" stackId="t" fill="#38bdf8" />
                    <Bar yAxisId="tokens" dataKey="输出" stackId="t" fill="#10b981" />
                    <Bar yAxisId="tokens" dataKey="缓存写入" stackId="t" fill="#f59e0b" radius={[3, 3, 0, 0]} />
                    <Line
                      yAxisId="calls"
                      type="monotone"
                      dataKey="调用"
                      stroke="#f43f5e"
                      strokeWidth={1.5}
                      strokeDasharray="5 3"
                      dot={false}
                    />
                  </ComposedChart>
                </ResponsiveContainer>
              </div>
            )}
          </>
        ) : (
          <>
            <div className="mb-3 grid grid-cols-2 gap-3 md:grid-cols-4">
              <div className="rounded-lg border-t-2 border-violet-500 bg-slate-50/60 p-3 dark:bg-zinc-900/60">
                <div className="text-xs text-slate-500">总 Tokens</div>
                <div className="text-xl font-bold tabular-nums text-violet-500">{sumTokens.toLocaleString()}</div>
                <div className="text-[11px] text-slate-400">输入 + 输出合计</div>
              </div>
              <div className="rounded-lg border-t-2 border-sky-500 bg-slate-50/60 p-3 dark:bg-zinc-900/60">
                <div className="text-xs text-slate-500">输入 Tokens</div>
                <div className="text-xl font-bold tabular-nums text-sky-500">{sumPrompt.toLocaleString()}</div>
                <div className="text-[11px] text-slate-400">prompt（含缓存读取）</div>
              </div>
              <div className="rounded-lg border-t-2 border-emerald-500 bg-slate-50/60 p-3 dark:bg-zinc-900/60">
                <div className="text-xs text-slate-500">输出 Tokens</div>
                <div className="text-xl font-bold tabular-nums text-emerald-500">{sumCompletion.toLocaleString()}</div>
                <div className="text-[11px] text-slate-400">completion 合计</div>
              </div>
              <div className="rounded-lg border-t-2 border-amber-500 bg-slate-50/60 p-3 dark:bg-zinc-900/60">
                <div className="text-xs text-slate-500">请求数</div>
                <div className="text-xl font-bold tabular-nums text-amber-500">{sumRequests.toLocaleString()}</div>
                <div className="text-[11px] text-slate-400">
                  {sumErrors > 0 ? `失败 ${sumErrors} 次` : '网关测量口径'}
                </div>
              </div>
            </div>
            {tokenTrend.length === 0 ? (
              <p className="py-4 text-center text-xs text-slate-400">该区间无网关请求记录。</p>
            ) : (
              <div className="h-56">
                <ResponsiveContainer width="100%" height="100%">
                  <ComposedChart data={tokenTrend} margin={{ top: 8, right: 8, bottom: 0, left: -8 }}>
                    <CartesianGrid strokeDasharray="3 3" stroke="#8884" vertical={false} />
                    <XAxis dataKey="date" tick={{ fontSize: 10 }} interval="preserveStartEnd" tickLine={false} axisLine={false} />
                    <YAxis
                      yAxisId="tokens"
                      tick={{ fontSize: 10 }}
                      tickLine={false}
                      axisLine={false}
                      tickFormatter={(v: number) => `${Math.round(v / 1000)}k`}
                    />
                    <YAxis yAxisId="calls" orientation="right" tick={{ fontSize: 10 }} tickLine={false} axisLine={false} />
                    <Tooltip contentStyle={{ fontSize: 12, borderRadius: 8 }} />
                    <Legend wrapperStyle={{ fontSize: 12 }} />
                    <Bar yAxisId="tokens" dataKey="输入" stackId="t" fill="#0ea5e9" />
                    <Bar yAxisId="tokens" dataKey="输出" stackId="t" fill="#10b981" radius={[3, 3, 0, 0]} />
                    <Line
                      yAxisId="calls"
                      type="monotone"
                      dataKey="调用"
                      stroke="#f43f5e"
                      strokeWidth={1.5}
                      strokeDasharray="5 3"
                      dot={false}
                    />
                  </ComposedChart>
                </ResponsiveContainer>
              </div>
            )}
          </>
        )}
      </div>
      )}

      {/* Token 模型排行（本地 = 按 tokens；网关 = 请求数口径，对齐国内诚实声明） */}
      {tab === 'tokens' && (
      <div className="card mt-4 p-4">
        <div className="mb-3 flex items-center justify-between">
          <span className="text-sm font-medium">模型消耗排行</span>
          <span className="text-[11px] text-slate-400">
            {tokenRange === 'today'
              ? '今日'
              : tokenRange === '7d'
                ? '近 7 天'
                : tokenRange === 'month'
                  ? '本月'
                  : tokenRange === 'year'
                    ? '近 90 天'
                    : '近 30 天'}{' '}
            · {tokensSource === 'local' ? '本地会话 · 按 tokens 排序' : '网关记账 · 按请求数排序'} Top8 + 其余合计
          </span>
        </div>
        {tokensSource === 'local' ? (
          tokenLocalRank.length === 0 ? (
            <p className="py-6 text-center text-xs text-slate-400">
              该区间无本地会话记录（通过 WorkBuddy AI 国际客户端产生会话后展示）。
            </p>
          ) : (
            <div className="space-y-2">
              {tokenLocalRank.slice(0, 8).map((m) => (
                <div key={m.name} className="flex items-center gap-3">
                  <span className="w-44 shrink-0 truncate text-xs text-slate-600 dark:text-zinc-300" title={m.name}>
                    {m.name}
                  </span>
                  <div className="h-2.5 flex-1 overflow-hidden rounded-full bg-slate-100 dark:bg-zinc-800">
                    <div
                      className="h-full rounded-full bg-sky-500"
                      style={{ width: `${tokenLocalRankMax > 0 ? Math.max((m.tokens / tokenLocalRankMax) * 100, 2) : 0}%` }}
                    />
                  </div>
                  <span className="w-24 shrink-0 text-right text-xs tabular-nums text-slate-500">
                    {m.tokens.toLocaleString()} tokens
                  </span>
                </div>
              ))}
              {tokenLocalRank.length > 8 && (
                <div className="flex items-center gap-3 border-t border-slate-100 pt-2 dark:border-zinc-800">
                  <span className="w-44 shrink-0 truncate text-xs text-slate-400">
                    其余 {tokenLocalRank.length - 8} 个模型
                  </span>
                  <span className="w-24 shrink-0 text-right text-xs tabular-nums text-slate-400">
                    {tokenLocalRank.slice(8).reduce((s, m) => s + m.tokens, 0).toLocaleString()} tokens
                  </span>
                </div>
              )}
              <p className="text-[11px] leading-4 text-slate-400">
                口径说明：本地源按会话 jsonl 的 tokens 字段逐日按模型聚合（~/.workbuddy-ai/projects）。
              </p>
            </div>
          )
        ) : tokenModelRank.length === 0 ? (
          <p className="py-6 text-center text-xs text-slate-400">该区间无网关请求记录。</p>
        ) : (
          <div className="space-y-2">
            {tokenModelRank.slice(0, 8).map((m) => (
              <div key={m.name} className="flex items-center gap-3">
                <span className="w-44 shrink-0 truncate text-xs text-slate-600 dark:text-zinc-300" title={m.name}>
                  {m.name}
                </span>
                <div className="h-2.5 flex-1 overflow-hidden rounded-full bg-slate-100 dark:bg-zinc-800">
                  <div
                    className="h-full rounded-full bg-sky-500"
                    style={{ width: `${tokenRankMax > 0 ? Math.max((m.requests / tokenRankMax) * 100, 2) : 0}%` }}
                  />
                </div>
                <span className="w-24 shrink-0 text-right text-xs tabular-nums text-slate-500">
                  {m.requests.toLocaleString()} 请求
                </span>
              </div>
            ))}
            {tokenModelRank.length > 8 && (
              <div className="flex items-center gap-3 border-t border-slate-100 pt-2 dark:border-zinc-800">
                <span className="w-44 shrink-0 truncate text-xs text-slate-400">
                  其余 {tokenModelRank.length - 8} 个模型
                </span>
                <span className="w-24 shrink-0 text-right text-xs tabular-nums text-slate-400">
                  {tokenModelRank.slice(8).reduce((s, m) => s + m.requests, 0).toLocaleString()} 请求
                </span>
              </div>
            )}
            <p className="text-[11px] leading-4 text-slate-400">
              口径说明：网关源无按模型 token 明细，排行按请求数计（对齐国内网关源口径）；
              模型粒度真实积分见「积分统计」Tab 排行。
            </p>
          </div>
        )}
      </div>
      )}

      {/* Token 年度活动热力图（本地 = 会话扫描全窗口；网关 = 90 天记账窗口） */}
      {tab === 'tokens' && (
      <div className="card mt-4 p-4">
        <div className="mb-3 flex items-center justify-between">
          <span className="text-sm font-medium">年度活动热力图</span>
          <span className="text-[11px] text-slate-400">
            每日 Tokens（输入 + 输出）·{' '}
            {tokensSource === 'local' ? '本地会话扫描（随扫描窗口）' : '网关记账（仅保留 90 天窗口）'}
          </span>
        </div>
        <ActivityHeatmap
          values={tokensSource === 'local' ? tokenLocalHeat : tokenHeat}
          unit="tokens"
          emptyHint={
            tokensSource === 'local'
              ? '暂无本地会话用量：通过 WorkBuddy AI 国际客户端产生会话后展示。'
              : '暂无网关用量：启用 buddy-ai 国际档并通过网关调用后展示。'
          }
        />
      </div>
      )}

      {/* 明细表 */}
      {tab === 'credits' && (
      <div className="card mt-4 overflow-x-auto p-4">
        {loading && accounts === null ? (
          <div className="py-10 text-center text-sm text-slate-400">加载中…</div>
        ) : listF.length === 0 ? (
          <div className="flex h-40 items-center justify-center text-sm text-slate-400">
            {/* 审查修复（M9 同款）：引导文案与账号管理页实际按钮实名对齐
                （「扫码登录/导入凭证」均非该页按钮名，用户照提示找不到入口）；
                审查修复：docker 版已裁剪「扫描本机账号」入口，引导句同步移除该表述 */}
            {list.length === 0
              ? '暂无国际账号，先到「账号管理」点「OAuth登录」授权入池，或「导入账号」。'
              : '当前筛选条件下无账号。'}
          </div>
        ) : (
          <table className="w-full min-w-[680px] text-sm">
            <thead>
              <tr className="border-b border-slate-100 text-left text-xs text-slate-400 dark:border-zinc-800">
                <th className="py-2 pr-3 font-medium">账号</th>
                <th className="py-2 pr-3 font-medium">积分余额</th>
                <th className="py-2 pr-3 font-medium">积分到期</th>
                <th className="py-2 pr-3 font-medium">最近取数</th>
              </tr>
            </thead>
            <tbody>
              {listF.map((a) => {
                const expired = a.credits_expire_at != null && a.credits_expire_at <= nowSec;
                return (
                  <tr key={a.id} className="border-b border-slate-50 last:border-0 dark:border-zinc-800/50">
                    <td className="py-2.5 pr-3">
                      <div className="font-medium">{a.nickname || a.id}</div>
                      <div className="text-[11px] text-slate-400">{a.uid || a.id}</div>
                    </td>
                    <td className="py-2.5 pr-3 tabular-nums">
                      {a.credits_balance != null ? (
                        <span className="inline-flex items-center gap-1">
                          <Coins size={13} className="text-amber-500" />
                          {a.credits_balance.toFixed(2)}
                        </span>
                      ) : (
                        <span className="text-xs text-slate-400">未取数</span>
                      )}
                    </td>
                    <td className="py-2.5 pr-3 text-xs tabular-nums">
                      <span className={expired ? 'text-rose-500' : 'text-slate-500'}>
                        {fmtExpire(a.credits_expire_at)}
                        {expired ? '（已过期）' : ''}
                      </span>
                    </td>
                    <td className="py-2.5 pr-3 text-xs text-slate-500">{fmtFetched(a.credits_fetched_at)}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
      )}

      {/* 模型消耗排行（积分口径对齐国内：Top8 + 其余合计；官网源 = 31 天全窗口汇总） */}
      {tab === 'credits' && (
      <div className="card mt-4 p-4">
        <div className="mb-3 flex items-center justify-between">
          <span className="text-sm font-medium">模型消耗排行</span>
          <span className="text-[11px] text-slate-400">
            {creditsSource === 'gateway'
              ? '近 90 天 · 网关记账 · 按积分排序 Top8 + 其余合计'
              : creditsSource === 'official'
                ? '官网 31 天全窗口 · 跨账号汇总 · 按积分排序'
                : '本地快照无模型明细'}
          </span>
        </div>
        {creditsSource === 'local' ? (
          <p className="py-6 text-center text-xs text-slate-400">
            本地快照仅有日合计（余额差分），无模型明细；切「官网」或「API网关」查看模型排行。
          </p>
        ) : (creditsSource === 'gateway' ? modelRank : officialModelRank).length === 0 ? (
          <p className="py-6 text-center text-xs text-slate-400">
            {creditsSource === 'official'
              ? '官网暂无模型消耗记录（31 天窗口），点「刷新数据」重拉后再试。'
              : '暂无消耗记录（启用 buddy-ai 国际档并通过网关调用后，此处展示按模型排行）。'}
          </p>
        ) : (
          <div className="space-y-2">
            {(creditsSource === 'gateway' ? modelRank : officialModelRank).slice(0, 8).map((m) => (
              <div key={m.name} className="flex items-center gap-3">
                <span className="w-44 shrink-0 truncate text-xs text-slate-600 dark:text-zinc-300" title={m.name}>
                  {m.name}
                </span>
                <div className="h-2.5 flex-1 overflow-hidden rounded-full bg-slate-100 dark:bg-zinc-800">
                  <div
                    className="h-full rounded-full bg-amber-500"
                    style={{
                      // 守门与分母同源：网关用网关 max，官网用官网 max（跨源混用会在
                      // 网关零用量时把官网排行整列渲染成 0%）
                      width: `${
                        (creditsSource === 'gateway' ? rankMax : officialModelRank[0]?.credits ?? 0) > 0
                          ? Math.max(
                              (m.credits / (creditsSource === 'gateway' ? rankMax : officialModelRank[0]?.credits ?? 0)) * 100,
                              2,
                            )
                          : 0
                      }%`,
                    }}
                  />
                </div>
                <span className="w-28 shrink-0 text-right text-xs tabular-nums text-slate-500">
                  {m.credits.toFixed(2)} 积分
                </span>
              </div>
            ))}
            {(creditsSource === 'gateway' ? modelRank : officialModelRank).length > 8 && (
              <div className="flex items-center gap-3 border-t border-slate-100 pt-2 dark:border-zinc-800">
                <span className="w-44 shrink-0 truncate text-xs text-slate-400">
                  其余 {(creditsSource === 'gateway' ? modelRank : officialModelRank).length - 8} 个模型
                </span>
                <span className="w-28 shrink-0 text-right text-xs tabular-nums text-slate-400">
                  {(creditsSource === 'gateway' ? modelRank : officialModelRank)
                    .slice(8)
                    .reduce((s, m) => s + m.credits, 0)
                    .toFixed(2)}{' '}
                  积分
                </span>
              </div>
            )}
            <p className="text-[11px] leading-4 text-slate-400">
              口径说明：
              {creditsSource === 'gateway'
                ? '按模型积分取自网关记账（模型倍率 × 用量）按日聚合；请求级真实积分见「会话明细」（官网流水）。'
                : '官网源为 get-user-request-usage 31 天全窗口跨账号汇总，不随区间筛选变化；请求级明细见「会话明细」。'}
            </p>
          </div>
        )}
      </div>
      )}

      {/* 年度活动热力图（消耗口径，随数据源切换；窗口诚实标注） */}
      {tab === 'credits' && (
      <div className="card mt-4 p-4">
        <div className="mb-3 flex items-center justify-between">
          <span className="text-sm font-medium">年度活动热力图</span>
          <span className="text-[11px] text-slate-400">
            每日消耗积分 ·{' '}
            {creditsSource === 'gateway'
              ? '网关记账（仅保留 90 天窗口）'
              : creditsSource === 'official'
                ? '官网流水（31 天窗口）'
                : '本地快照差分（随快照保留窗口）'}
          </span>
        </div>
        <ActivityHeatmap
          values={heatValues}
          unit="积分"
          emptyHint={
            creditsSource === 'official'
              ? '官网源暂无消耗记录：点「刷新数据」重拉后再试。'
              : '暂无消耗记录：启用 buddy-ai 国际档并通过网关调用后展示。'
          }
        />
      </div>
      )}

      {/* 会话明细（定时落地口径：官网请求级行缓存，纯缓存零网络；对齐国内 Buddy SessionsTab） */}
      {tab === 'sessions' && (
      <div className="mt-4 space-y-4">
        {/* 筛选行：日期区间 + 快捷区间 + 账号 / 模型下拉（默认全部账号） */}
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-xs font-medium text-slate-500 dark:text-zinc-400">筛选</span>
          <input
            type="date"
            value={sessStart}
            onChange={(e) => {
              setSessStart(e.target.value);
              setSessPage(1);
            }}
            className="input !py-1.5 !text-xs w-36"
          />
          <span className="text-xs text-slate-400">至</span>
          <input
            type="date"
            value={sessEnd}
            onChange={(e) => {
              setSessEnd(e.target.value);
              setSessPage(1);
            }}
            className="input !py-1.5 !text-xs w-36"
          />
          <button
            className="chip border"
            onClick={() => {
              setSessStart(sessDate(6));
              setSessEnd(sessDate(0));
              setSessPage(1);
            }}
          >
            近 7 天
          </button>
          <button
            className="chip border"
            onClick={() => {
              setSessStart(sessDate(29));
              setSessEnd(sessDate(0));
              setSessPage(1);
            }}
          >
            近 30 天
          </button>
          <select
            value={sessUser}
            onChange={(e) => {
              setSessUser(e.target.value);
              setSessPage(1);
            }}
            className="input !w-auto !py-1 text-xs"
          >
            <option value="">全部账号</option>
            {listF.map((a) => (
              <option key={a.id} value={a.id}>
                {a.nickname || a.id}
              </option>
            ))}
          </select>
          <select
            value={sessModel}
            onChange={(e) => {
              setSessModel(e.target.value);
              setSessPage(1);
            }}
            className="input !w-auto !py-1 text-xs"
          >
            <option value="">全部模型</option>
            {sessModels.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
          {sessLoading && <span className="text-xs text-slate-400">加载中…</span>}
          {sessData && sessData.fetched_at > 0 && (
            <span className="text-[11px] text-slate-400">
              落地于 {new Date(sessData.fetched_at * 1000).toLocaleString()}
            </span>
          )}
        </div>

        {sessData && sessData.total === 0 ? (
          <div className="card p-4">
            <p className="py-6 text-center text-xs text-slate-400">
              {sessData.fetched_at > 0
                ? '当前筛选条件下暂无会话记录，可调整时间范围、账号或模型后重试。'
                : '暂无落地数据：官网请求级行随「积分统计 · 官网源」加载或定时同步（Buddy 国际版积分数据同步）落盘，完成后自动展示。'}
            </p>
          </div>
        ) : (
          <div className="card overflow-hidden">
            <div className="flex items-center justify-between border-b border-slate-100 px-5 py-3 dark:border-zinc-800">
              <h3 className="font-medium">会话明细</h3>
              <span className="text-xs text-slate-400">
                {sessData
                  ? `共 ${sessData.total.toLocaleString()} 条请求 · 区间合计 ${sessData.credits_total.toFixed(2)} 积分`
                  : ''}
              </span>
            </div>
            <div className="overflow-x-auto">
              <table className="w-full min-w-[720px] text-sm">
                <thead className="bg-slate-50 text-xs uppercase text-slate-500 dark:bg-zinc-900">
                  <tr>
                    <th className="px-4 py-2 text-left">时间</th>
                    <th className="px-4 py-2 text-left">账号</th>
                    <th className="px-4 py-2 text-left">会话说明</th>
                    <th className="px-4 py-2 text-left">模型</th>
                    <th className="px-4 py-2 text-right">消耗积分</th>
                  </tr>
                </thead>
                <tbody>
                  {(sessData?.rows ?? []).map((r, idx) => (
                    <tr
                      key={`${r.user_id}-${r.session_id || 'x'}-${r.ts}-${idx}`}
                      className="border-t border-slate-200 dark:border-zinc-800"
                    >
                      <td className="whitespace-nowrap px-4 py-2 text-xs tabular-nums text-slate-500">
                        {new Date(r.ts * 1000).toLocaleString()}
                      </td>
                      <td className="whitespace-nowrap px-4 py-2 text-xs">{r.account_name || r.user_id}</td>
                      <td className="max-w-[260px] truncate px-4 py-2 text-xs" title={r.preview}>
                        {r.preview || '—'}
                      </td>
                      <td className="whitespace-nowrap px-4 py-2 text-xs">{r.model}</td>
                      <td className="px-4 py-2 text-right text-xs tabular-nums">
                        {r.credits > 0 ? (
                          <span className="text-amber-600 dark:text-amber-400">{r.credits.toFixed(2)}</span>
                        ) : (
                          <span className="text-slate-400">0</span>
                        )}
                      </td>
                    </tr>
                  ))}
                  {sessData && sessData.rows.length === 0 && (
                    <tr>
                      <td colSpan={5} className="px-4 py-6 text-center text-xs text-slate-400">
                        当前筛选条件下无会话记录
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </div>
            <div className="flex items-center justify-between border-t border-slate-100 px-5 py-2.5 dark:border-zinc-800">
              <span className="text-xs text-slate-400">
                第 {sessData?.page ?? sessPage} / {sessData ? Math.max(1, Math.ceil(sessData.total / sessData.page_size)) : 1} 页 · 每页 50 条
              </span>
              <div className="flex gap-2">
                <button
                  className="btn-outline !px-2.5 !py-1 text-xs"
                  disabled={sessLoading || sessPage <= 1}
                  onClick={() => setSessPage((p) => Math.max(1, p - 1))}
                >
                  上一页
                </button>
                <button
                  className="btn-outline !px-2.5 !py-1 text-xs"
                  disabled={
                    sessLoading || !sessData || sessPage >= Math.max(1, Math.ceil(sessData.total / sessData.page_size))
                  }
                  onClick={() => setSessPage((p) => p + 1)}
                >
                  下一页
                </button>
              </div>
            </div>
          </div>
        )}
      </div>
      )}
    </div>
  );
}
