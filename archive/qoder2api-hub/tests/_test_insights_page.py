"""用量洞察独立板块：导航、图标卡片、动画与可访问性守卫。"""
import io
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["dashboard.html"])

_dash = io.open(os.path.join(ROOT, "dashboard.html"), encoding="utf-8").read()

print("[insights-page] 已并入数据指标看板")
check("导航栏不再单列「用量洞察」页签", 'id="btnNavInsights"' not in _dash)
check("主标签列表不含 insights",
      "'insights'" not in _dash.split("const MAIN_TABS =")[1][:200])
check("独立的 #pageInsights 已删除", 'id="pageInsights"' not in _dash)
check("洞察区块挂在数据看板页内",
      'id="sectionInsights"' in _dash
      and _dash.index('id="sectionInsights"') > _dash.index('id="pageAnalytics"'))
check("切到数据看板时一并加载洞察",
      "if(tab === 'analytics') loadInsights();" in _dash)
check("网关页不再内嵌洞察区块",
      'id="insightSection"' not in _dash)

print("[insights-page] 图标卡片与动画")
check("四种图标已定义",
      all(k in _dash for k in ("cost:", "grants:", "days:", "activity:")))
check("卡片渲染抽成 _insightCard",
      "function _insightCard(" in _dash and "insight-icon" in _dash)
check("数字滚动动画（_countUp + rAF）",
      "function _countUp(" in _dash and "requestAnimationFrame(step)" in _dash)
check("尊重 reduced-motion",
      "prefers-reduced-motion" in _dash and "_prefersReduced()" in _dash)
check("进度条宽度过渡", "insight-bar>i" in _dash and "transition:width" in _dash)
check("卡片悬停抬升 + 图标缩放",
      ".insight-card:hover{transform:translateY(-3px)" in _dash
      and ".insight-card:hover .insight-icon" in _dash)
check("表格行悬停与入场 stagger",
      ".insight-table tbody tr:hover" in _dash
      and "insightRowIn" in _dash and "--i" in _dash)
check("刷新按钮有旋转态", "#btnInsightRefresh.spin svg" in _dash)
check("全局按钮/导航过渡",
      ".main-nav-btn:active" in _dash and "button:not(:disabled):active" in _dash)
check("焦点可见样式", ":focus-visible" in _dash)

print("[insights-page] 空状态与提示")
check("空状态组件 _empty", "function _empty(" in _dash)
check("三张表都有空状态文案",
      _dash.count("_empty(") >= 4, _dash.count("_empty("))
check("表头带计数提示（grantsHint/remainingHint/activityHint）",
      all(k in _dash for k in ("grantsHint", "remainingHint", "activityHint")))
check("洞察卡片容器已并入看板页",
      _dash.index('id="insightCards"') > _dash.index('id="pageAnalytics"'))
check("洞察 section 有自己的 h2（左侧导航可锚定）",
      '<section id="sectionInsights">' in _dash and '用量洞察' in _dash)

print("[insights-page] 侧栏折叠按页面记忆")
check("折叠 key 按页面区分（不再全局一个键）",
      "function pageNavCollapseKey(page){" in _dash
      and "PAGE_NAV_COLLAPSE_KEY + ':' + ((page && page.id) || 'page')" in _dash)
check("没有记录时默认展开",
      "if(v === null) return false;" in _dash)
check("toggle 写的是本页的键",
      "localStorage.setItem(pageNavCollapseKey(page)" in _dash)
check("初始化时读本页偏好",
      "applyPageNavCollapse(page, pageNavCollapsedPref(page));" in _dash)

finish()