"""Regression checks for the credit reconciliation indicators in the web console."""

from __future__ import annotations

import re

from src import main


def test_console_exposes_credit_calibration_state():
    html = main._web_login_html()
    for token in (
        'id="credits-calibration-state"',
        'id="stat-credits-meta"',
        'id="ov-credits-meta"',
        "creditUiState",
        "credits_reconciled_at",
        "credits_reconcile_status",
        "credits_reconcile_confidence",
        "credits_official_remaining",
        "credits_local_debit",
        "creditStatusLabel",
        "加权估算",
        "待校准",
    ):
        assert token in html


def test_console_reconciles_new_settled_usage_once_without_fast_upstream_timer():
    html = main._web_login_html()
    usage_start = html.index("function usageCreditFingerprint")
    usage_end = html.index("async function refreshUsage()", usage_start)
    usage_source = html[usage_start:usage_end]
    assert "credits_consumed" in usage_source
    assert "creditUiState.usageFingerprintReady" in usage_source
    assert "creditsRefreshAll({silent:true,reason:'usage'})" in usage_source

    # Usage history still polls frequently for display, but credit provider
    # refreshes are event-driven/debounced and not attached to a 5s interval.
    assert not re.search(r"setInterval\s*\(\s*creditsRefreshAll\s*,\s*5000", html)
    assert "refreshOverview();" in html
    assert "never calls the" in html


def test_console_accepts_old_and_reconciled_credit_shapes():
    html = main._web_login_html()
    set_credits_start = html.index("function setCredits")
    set_credits_end = html.index("function setCheckinState", set_credits_start)
    source = html[set_credits_start:set_credits_end]
    snapshot_start = html.index("function creditSnapshot")
    snapshot_end = html.index("function creditStatusLabel", snapshot_start)
    assert "data.account_credits||data.credits||data" in html[snapshot_start:snapshot_end]
    assert "value.remaining" in source
    assert "value.official_remaining" in source
    assert "row.dataset.creditLocalDebit" in source

    overview_start = html.index("async function refreshOverview()")
    overview_end = html.index("function renderOverview()", overview_start)
    assert "updateCreditCalibrationMeta" not in html[overview_start:overview_end]
    # The renderer itself updates the calibration metadata after reading the
    # overview account aggregate, preserving compatibility with old payloads.
    render_start = html.index("function renderOverview()")
    render_end = html.index("function renderOverviewChart()", render_start)
    assert "updateCreditCalibrationMeta(a)" in html[render_start:render_end]

