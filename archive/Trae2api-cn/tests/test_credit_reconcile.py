import asyncio
import json
import os
import time
import unittest
from unittest.mock import AsyncMock, patch

from src import auth, main


class CreditReconcileTests(unittest.TestCase):
    def setUp(self):
        self.accounts = {
            "account-1": {
                "user_id": "account-1",
                "token": "token",
                "checkin": {
                    "account_credits": {
                        "total_limit": 100,
                        "used": 10,
                        "remaining": 90,
                    }
                },
            }
        }
        self.history = list(main._USAGE_HISTORY)
        self.reconcile_state = dict(main._CREDIT_RECONCILE_STATE)
        self.now = time.time()
        main._USAGE_HISTORY = []
        main._CREDIT_RECONCILE_STATE.clear()
        self.account_patch = patch.object(auth, "_accounts", self.accounts)
        self.save_patch = patch.object(auth, "_save_accounts")
        self.account_patch.start()
        self.save_patch.start()

    def tearDown(self):
        self.save_patch.stop()
        self.account_patch.stop()
        main._USAGE_HISTORY = self.history
        main._CREDIT_RECONCILE_STATE.clear()
        main._CREDIT_RECONCILE_STATE.update(self.reconcile_state)

    def _row(self, amount, source="session_usage", settled_at=None):
        return {
            "account_id": "account-1",
            "request_id": f"req-{amount}-{source}",
            "credits_consumed": amount,
            "credits_source": source,
            "credits_settled_at": settled_at or self.now + 5,
            "timestamp": self.now,
        }

    def test_snapshot_keeps_overlay_when_upstream_bill_lags(self):
        auth.reconcile_account_credits(
            "account-1",
            {
                "account_credits": {
                    "total_limit": 100,
                    "used": 10,
                    "remaining": 90,
                }
            },
            observed_at=self.now,
        )
        main._USAGE_HISTORY = [self._row(2)]
        state, projected = main._credit_snapshot_projection(
            "account-1",
            {"used": 10, "remaining": 90, "total_limit": 100},
            self.now + 10,
        )
        self.assertEqual(projected["effective_remaining"], 88)
        self.assertEqual(state["local_debit"], 2)
        self.assertEqual(state["status"], "projected")

    def test_snapshot_absorbs_overlay_when_upstream_used_advances(self):
        auth.reconcile_account_credits(
            "account-1",
            {
                "account_credits": {
                    "total_limit": 100,
                    "used": 10,
                    "remaining": 90,
                }
            },
            observed_at=self.now,
        )
        main._USAGE_HISTORY = [self._row(2)]
        state, projected = main._credit_snapshot_projection(
            "account-1",
            {"used": 12, "remaining": 88, "total_limit": 100},
            self.now + 10,
        )
        self.assertEqual(projected["effective_remaining"], 88)
        self.assertEqual(state["local_debit"], 0)
        self.assertEqual(state["upstream_delta"], 2)

    def test_pending_rows_do_not_reduce_projected_balance(self):
        auth.reconcile_account_credits(
            "account-1",
            {
                "account_credits": {
                    "total_limit": 100,
                    "used": 10,
                    "remaining": 90,
                }
            },
            observed_at=self.now,
        )
        main._USAGE_HISTORY = [self._row(2, source="pending")]
        credits = main._refresh_credit_projection("account-1")
        self.assertEqual(credits["remaining"], 90)
        self.assertEqual(credits["local_debit"], 0)

    def test_usage_update_records_balance_after_settlement(self):
        auth.reconcile_account_credits(
            "account-1",
            {
                "account_credits": {
                    "total_limit": 100,
                    "used": 10,
                    "remaining": 90,
                }
            },
            observed_at=self.now,
        )
        main._USAGE_HISTORY = [
            main._normalize_usage_record(
                {
                    "account_id": "account-1",
                    "request_id": "req-1",
                    "model": "m",
                    "timestamp": self.now,
                    "credits_source": "pending",
                }
            )
        ]
        main._update_usage_record(
            "req-1",
            credits_consumed=1.5,
            credits_source="session_usage",
        )
        row = main._USAGE_HISTORY[0]
        self.assertEqual(row["credits_remaining_after"], 88.5)
        self.assertEqual(
            auth.get_account_credit_state("account-1")["remaining"], 88.5
        )

    def test_auth_reconcile_persists_official_and_effective_fields(self):
        merged = auth.reconcile_account_credits(
            "account-1",
            {
                "account_credits": {
                    "total_limit": 100,
                    "used": 11,
                    "remaining": 89,
                }
            },
            effective_remaining=88.5,
            local_debit=0.5,
            confidence=0.85,
            source="upstream_weighted",
            status="projected",
            observed_at=123.0,
        )
        credits = merged["account_credits"]
        self.assertEqual(credits["official_remaining"], 89)
        self.assertEqual(credits["remaining"], 88.5)
        self.assertEqual(credits["local_debit"], 0.5)
        self.assertEqual(credits["reconcile_confidence"], 0.85)
        self.assertEqual(credits["credits_anchor_at"], 123.0)

    def test_reconcile_cycle_refreshes_accounts_and_marks_failures_stale(self):
        accounts = [
            ("no-token", {}),
            ("account-ok", {"token": "ok"}),
            ("account-failed", {"token": "failed"}),
        ]
        refreshed = AsyncMock(
            side_effect=[
                {"account_credits": {"remaining": 8}},
                RuntimeError("provider unavailable"),
            ]
        )
        stale = []
        with (
            patch.object(auth, "get_accounts_raw", return_value=accounts),
            patch.object(main, "_fetch_credit_account_snapshot", new=refreshed),
            patch.object(
                auth,
                "mark_account_credit_reconcile_stale",
                side_effect=lambda aid: stale.append(aid) or {},
            ),
        ):
            summary = asyncio.run(main._credit_reconcile_cycle("test"))

        self.assertEqual(summary, {"total": 3, "ok": 1, "failed": 1, "skipped": 1})
        self.assertEqual(refreshed.await_count, 2)
        self.assertEqual(stale, ["account-failed"])
        self.assertEqual(main._CREDIT_RECONCILE_STATE["account-ok"]["status"], "ok")
        self.assertEqual(main._CREDIT_RECONCILE_STATE["account-failed"]["status"], "stale")

    def test_reconcile_loop_propagates_cancellation_after_initial_delay(self):
        cycle = AsyncMock(side_effect=asyncio.CancelledError())
        with (
            patch.dict(
                os.environ,
                {
                    "TRAE_CREDIT_RECONCILE_INITIAL_DELAY_SECONDS": "0",
                    "TRAE_CREDIT_RECONCILE_INTERVAL_SECONDS": "15",
                },
                clear=False,
            ),
            patch.object(main, "_credit_reconcile_cycle", new=cycle),
        ):
            with self.assertRaises(asyncio.CancelledError):
                asyncio.run(main._credit_reconcile_loop())
        cycle.assert_awaited_once_with("schedule")

    def test_overview_surfaces_stale_aggregate_status(self):
        accounts = [
            {
                "is_valid": True,
                "checked_in": True,
                "account_credits": {
                    "remaining": 40,
                    "official_remaining": 40,
                    "reconcile_status": "stale",
                },
                "credits_reconcile_status": "stale",
                "credits_reconciled_at": self.now,
                "credits_reconcile_confidence": 1.0,
            }
        ]
        with (
            patch.object(auth, "list_accounts", return_value=accounts),
            patch.object(auth, "get_polling_status", return_value={"enabled": False}),
            patch.object(auth, "get_active_account_id", return_value="account-1"),
            patch.object(auth, "get_max_mode_settings", return_value={"enabled": False}),
            patch.object(auth, "get_auto_checkin_settings", return_value={"enabled": False}),
            patch.object(auth, "get_auth", return_value=type("Auth", (), {"is_valid": lambda self: True})()),
            patch.object(main, "_usage_overview", return_value={"days": []}),
        ):
            response = asyncio.run(main.api_overview())

        self.assertEqual(response.status_code, 200)
        self.assertEqual(
            json.loads(response.body)["accounts"]["credits_reconcile_status"],
            "stale",
        )


if __name__ == "__main__":
    unittest.main()
