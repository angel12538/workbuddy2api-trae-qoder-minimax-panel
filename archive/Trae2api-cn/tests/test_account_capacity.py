from __future__ import annotations

import asyncio
import unittest
from unittest.mock import AsyncMock, patch

from src import auth, main as main_module, raw_client, trae_client


def _token(account_id: str) -> str:
    return f"header.{account_id}{'p' * 140}.signature"


def _record(account_id: str, remaining: int, **extra) -> dict:
    record = {
        "user_id": account_id,
        "token": _token(account_id),
        "source": "web-login",
        "expired_at": "2099-01-01T00:00:00Z",
        "provider_specific": {},
        "checkin": {"account_credits": {"remaining": remaining}},
    }
    record.update(extra)
    return record


class AccountCapacityTests(unittest.TestCase):
    def setUp(self):
        self.accounts = {
            "rich": _record("rich", 100),
            "poor": _record("poor", 10),
        }
        self.patches = [
            patch.object(auth, "_accounts", self.accounts),
            patch.object(auth, "_active_account", "rich"),
            patch.object(auth, "_poll_enabled", True),
            patch.object(auth, "_polling_mode", "credit-priority"),
            patch.object(auth, "_rotation_cursor", 0),
            patch.object(auth, "_MODEL_ACCOUNT_INFLIGHT", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVED", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVATION_OWNERS", {}),
            patch.object(auth, "_save_accounts"),
        ]
        for item in self.patches:
            item.start()

    def tearDown(self):
        for item in reversed(self.patches):
            item.stop()

    def test_free_accounts_get_two_slots_and_credit_priority_fills_rich_first(self):
        chosen = [auth.reserve_model_account()[0] for _ in range(4)]
        self.assertEqual(chosen, ["rich", "rich", "poor", "poor"])
        self.assertEqual(auth.reserve_model_account(), ("", {}))
        self.assertEqual(auth.account_capacity_snapshot("rich")["inflight"], 2)
        self.assertEqual(auth.account_capacity_snapshot("poor")["inflight"], 2)

        for aid in chosen:
            self.assertTrue(auth.cancel_account_slot_reservation(aid))
        self.assertEqual(auth.account_capacity_snapshot("rich")["inflight"], 0)
        self.assertEqual(auth.account_capacity_snapshot("poor")["available"], 2)

    def test_retry_excludes_failed_rich_account_in_credit_priority(self):
        attempted = {
            main_module._retry_account_key(
                {
                    "_account_id": "rich",
                    "_auth_token": self.accounts["rich"]["token"],
                },
                0,
            )
        }
        account_id, snapshot = main_module._next_retry_account_snapshot(
            attempted, 2
        )
        self.assertEqual(account_id, "poor")
        self.assertTrue(snapshot.get("_slot_reservation_id"))
        self.assertEqual(auth.account_capacity_snapshot("rich")["inflight"], 0)
        self.assertEqual(auth.account_capacity_snapshot("poor")["inflight"], 1)
        self.assertTrue(
            auth.cancel_account_slot_reservation(
                account_id, snapshot["_slot_reservation_id"]
            )
        )

    def test_retry_uses_legacy_inventory_when_store_rows_do_not_match(self):
        with (
            patch.object(
                auth,
                "list_accounts",
                return_value=[
                    {"id": "legacy-a", "is_valid": True},
                    {"id": "legacy-b", "is_valid": True},
                ],
            ),
            patch.object(auth, "next_polling_account") as rotate,
            patch.object(
                auth,
                "get_active_account_snapshot",
                return_value=("legacy-b", {"token": ""}),
            ),
        ):
            snapshot = main_module._next_retry_account_snapshot({"legacy-a"}, 2)

        self.assertEqual(snapshot, ("legacy-b", {"token": ""}))
        rotate.assert_called_once_with()
        self.assertEqual(auth.account_capacity_snapshot("rich")["inflight"], 0)
        self.assertEqual(auth.account_capacity_snapshot("poor")["inflight"], 0)

    def test_explicit_entitlement_overrides_free_fallback(self):
        self.accounts["paid"] = _record(
            "paid",
            200,
            model_parallel_limit=5,
            account_tier="Pro",
            pay_identity=1,
        )
        self.assertEqual(auth.account_parallel_limit("rich"), 2)
        self.assertEqual(auth.account_parallel_limit("paid"), 5)

    def test_list_accounts_exposes_identity_and_capacity(self):
        self.accounts["paid"] = _record(
            "paid",
            200,
            model_parallel_limit=7,
            account_tier="ProPlus",
            pay_identity=4,
            pay_identity_str="Pro+",
        )
        row = next(item for item in auth.list_accounts() if item["id"] == "paid")
        self.assertEqual(row["account_tier"], "ProPlus")
        self.assertEqual(row["pay_identity_str"], "Pro+")
        self.assertEqual(row["model_parallel_limit"], 7)
        self.assertEqual(row["capacity"]["available"], 7)

    def test_jwt_identity_alias_shares_the_canonical_account_pool(self):
        self.accounts["store-key"] = _record("jwt-owner", 50)
        self.assertEqual(auth.resolve_model_account_id("jwt-owner"), "store-key")
        self.assertTrue(auth.try_reserve_account_slot("jwt-owner"))
        self.assertEqual(
            auth.account_capacity_snapshot("store-key")["inflight"], 1
        )
        self.assertTrue(auth.release_account_slot("jwt-owner"))


class EntitlementParserTests(unittest.TestCase):
    def test_reads_identity_and_nested_subscription_parallel_limit(self):
        parsed = trae_client.parse_account_credits(
            {
                "user_pay_identity": 4,
                "user_pay_identity_str": "ProPlus",
                "user_entitlement_pack_list": [
                    {
                        "status": "active",
                        "entitlement_base_info": {
                            "product_id": "subscription",
                            "product_extra": {
                                "subscription_extra": {
                                    "quota": {
                                        "credits_limit": 100,
                                        "solo_agent_parallel_limit": 6,
                                    }
                                }
                            },
                        },
                        "usage": {"credits_amount": 2},
                    }
                ],
            }
        )
        self.assertEqual(parsed["remaining"], 98)
        self.assertEqual(parsed["solo_agent_parallel_limit"], 6)
        self.assertEqual(parsed["model_parallel_limit"], 6)
        self.assertEqual(parsed["user_pay_identity_str"], "ProPlus")


class KnownAccountWebSlotTests(unittest.IsolatedAsyncioTestCase):
    async def test_known_account_uses_dynamic_capacity_and_releases(self):
        record = _record("free", 0)
        with (
            patch.object(auth, "_accounts", {"free": record}),
            patch.object(auth, "_MODEL_ACCOUNT_INFLIGHT", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVED", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVATION_OWNERS", {}),
            patch.object(trae_client, "_ACCOUNT_SLOT_RESERVATIONS", {}),
            patch.object(trae_client, "_ACCOUNT_SLOT_LEASE_OWNERS", {}),
        ):
            first_lease = await trae_client.acquire_web_slot("free", timeout=0.01)
            second_lease = await trae_client.acquire_web_slot("free", timeout=0.01)
            self.assertFalse(trae_client.web_slot_available("free"))
            with self.assertRaisesRegex(RuntimeError, "account session slot busy"):
                await trae_client.acquire_web_slot("free", timeout=0.01)
            trae_client.release_web_slot("free", first_lease)
            self.assertTrue(trae_client.web_slot_available("free"))
            trae_client.release_web_slot("free", second_lease)

    async def test_unowned_release_cannot_free_another_transport_slot(self):
        record = _record("free", 0)
        with (
            patch.object(auth, "_accounts", {"free": record}),
            patch.object(auth, "_MODEL_ACCOUNT_INFLIGHT", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVED", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVATION_OWNERS", {}),
            patch.object(trae_client, "_ACCOUNT_SLOT_RESERVATIONS", {}),
            patch.object(trae_client, "_ACCOUNT_SLOT_LEASE_OWNERS", {}),
        ):
            first_lease = await trae_client.acquire_web_slot("free", timeout=0.01)
            second_lease = await trae_client.acquire_web_slot("free", timeout=0.01)

            async def unowned_release():
                trae_client.release_web_slot("free")

            await asyncio.create_task(unowned_release())
            trae_client.release_web_slot("free", "not-an-owner")
            self.assertEqual(auth.account_capacity_snapshot("free")["inflight"], 2)

            trae_client.release_web_slot("free", first_lease)
            self.assertEqual(auth.account_capacity_snapshot("free")["inflight"], 1)
            trae_client.release_web_slot("free", second_lease)
            self.assertEqual(auth.account_capacity_snapshot("free")["inflight"], 0)

    async def test_scheduler_reservation_is_transferred_without_double_counting(self):
        record = _record("free", 10)
        with (
            patch.object(auth, "_accounts", {"free": record}),
            patch.object(auth, "_active_account", "free"),
            patch.object(auth, "_MODEL_ACCOUNT_INFLIGHT", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVED", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVATION_OWNERS", {}),
            patch.object(trae_client, "_ACCOUNT_SLOT_RESERVATIONS", {}),
            patch.object(trae_client, "_ACCOUNT_SLOT_LEASE_OWNERS", {}),
        ):
            account_id, record_snapshot = auth.reserve_model_account()
            self.assertEqual(account_id, "free")
            self.assertEqual(auth.account_capacity_snapshot("free")["inflight"], 1)
            lease_token = await trae_client.acquire_web_slot(
                "free",
                timeout=0.01,
                reservation_id=record_snapshot["_slot_reservation_id"],
            )
            self.assertEqual(auth.account_capacity_snapshot("free")["inflight"], 1)
            trae_client.release_web_slot("free", lease_token)
            self.assertEqual(auth.account_capacity_snapshot("free")["inflight"], 0)

    async def test_unbound_transport_does_not_consume_another_request_reservation(self):
        record = _record("free", 10)
        with (
            patch.object(auth, "_accounts", {"free": record}),
            patch.object(auth, "_active_account", "free"),
            patch.object(auth, "_MODEL_ACCOUNT_INFLIGHT", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVED", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVATION_OWNERS", {}),
            patch.object(trae_client, "_ACCOUNT_SLOT_RESERVATIONS", {}),
            patch.object(trae_client, "_ACCOUNT_SLOT_LEASE_OWNERS", {}),
        ):
            account_id, reserved = auth.reserve_model_account()
            reservation_id = reserved["_slot_reservation_id"]
            lease_token = await trae_client.acquire_web_slot("free", timeout=0.01)
            self.assertEqual(auth.account_capacity_snapshot("free")["inflight"], 2)
            self.assertEqual(
                auth._MODEL_ACCOUNT_RESERVATION_OWNERS.get(reservation_id), "free"
            )
            trae_client.release_web_slot(account_id, lease_token)
            self.assertTrue(auth.cancel_account_slot_reservation("free", reservation_id))
            self.assertEqual(auth.account_capacity_snapshot("free")["inflight"], 0)

    async def test_exact_reservation_cleanup_cannot_cancel_another_request(self):
        record = _record("free", 10)
        with (
            patch.object(auth, "_accounts", {"free": record}),
            patch.object(auth, "_active_account", "free"),
            patch.object(auth, "_MODEL_ACCOUNT_INFLIGHT", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVED", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVATION_OWNERS", {}),
            patch.object(trae_client, "_ACCOUNT_SLOT_RESERVATIONS", {}),
            patch.object(trae_client, "_ACCOUNT_SLOT_LEASE_OWNERS", {}),
        ):
            first_id, first = auth.reserve_model_account()
            second_id, second = auth.reserve_model_account()
            first_reservation = first["_slot_reservation_id"]
            second_reservation = second["_slot_reservation_id"]

            first_lease = await trae_client.acquire_web_slot(
                first_id, timeout=0.01, reservation_id=first_reservation
            )
            self.assertFalse(
                auth.cancel_account_slot_reservation(
                    first_id, first_reservation
                )
            )
            self.assertEqual(auth.account_capacity_snapshot("free")["inflight"], 2)
            self.assertTrue(
                auth.cancel_account_slot_reservation(
                    second_id, second_reservation
                )
            )
            self.assertEqual(auth.account_capacity_snapshot("free")["inflight"], 1)
            trae_client.release_web_slot("free", first_lease)
            self.assertEqual(auth.account_capacity_snapshot("free")["inflight"], 0)

    async def test_raw_same_session_uses_two_account_slots_not_a_session_gate(self):
        record = _record("free", 10)
        with (
            patch.object(auth, "_accounts", {"free": record}),
            patch.object(auth, "_active_account", "free"),
            patch.object(auth, "_MODEL_ACCOUNT_INFLIGHT", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVED", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVATION_OWNERS", {}),
            patch.object(trae_client, "_ACCOUNT_SLOT_RESERVATIONS", {}),
            patch.object(trae_client, "_ACCOUNT_SLOT_LEASE_OWNERS", {}),
            patch.object(raw_client, "_RAW_SESSION_GATES", {}),
            patch.dict(
                "os.environ",
                {
                    "TRAE_MODEL_REQUEST_PARALLEL_LIMIT": "",
                    "TRAE_WEB_PARALLEL_LIMIT": "",
                    "TRAE_WEB_SLOT_TIMEOUT": "0.01",
                },
                clear=False,
            ),
        ):
            account_id, reserved = auth.reserve_model_account()
            release_first = await raw_client._acquire_raw_session_gate(
                "same-upstream-session",
                account_id,
                record["token"],
                reserved["_slot_reservation_id"],
            )
            release_second = await raw_client._acquire_raw_session_gate(
                "same-upstream-session", account_id, record["token"]
            )
            self.assertEqual(auth.account_capacity_snapshot("free")["inflight"], 2)
            self.assertEqual(raw_client._RAW_SESSION_GATES, {})
            with self.assertRaisesRegex(RuntimeError, "account session slot busy"):
                await raw_client._acquire_raw_session_gate(
                    "same-upstream-session", account_id, record["token"]
                )

            await asyncio.to_thread(release_first)
            release_second()
            self.assertEqual(auth.account_capacity_snapshot("free")["inflight"], 0)

    async def test_rebound_transport_cancels_stale_scheduler_reservation(self):
        accounts = {
            "first": _record("first", 20),
            "second": _record("second", 10),
        }
        with (
            patch.object(auth, "_accounts", accounts),
            patch.object(auth, "_active_account", "first"),
            patch.object(auth, "_MODEL_ACCOUNT_INFLIGHT", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVED", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVATION_OWNERS", {}),
            patch.object(trae_client, "_ACCOUNT_SLOT_RESERVATIONS", {}),
            patch.object(trae_client, "_ACCOUNT_SLOT_LEASE_OWNERS", {}),
        ):
            _account_id, reserved = auth.reserve_model_account()
            lease_token = await trae_client.acquire_web_slot(
                "second",
                timeout=0.01,
                reservation_id=reserved["_slot_reservation_id"],
            )
            self.assertEqual(auth.account_capacity_snapshot("first")["inflight"], 0)
            self.assertEqual(auth.account_capacity_snapshot("second")["inflight"], 1)
            trae_client.release_web_slot("second", lease_token)


class MainAccountReservationTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.accounts = {
            "rich": _record("rich", 100),
            "poor": _record("poor", 10),
        }
        self.patches = [
            patch.object(auth, "_accounts", self.accounts),
            patch.object(auth, "_active_account", "rich"),
            patch.object(auth, "_poll_enabled", True),
            patch.object(auth, "_polling_mode", "credit-priority"),
            patch.object(auth, "_rotation_cursor", 0),
            patch.object(auth, "_MODEL_ACCOUNT_INFLIGHT", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVED", {}),
            patch.object(auth, "_MODEL_ACCOUNT_RESERVATION_OWNERS", {}),
            patch.object(auth, "_save_accounts"),
            patch.object(main_module, "UPSTREAM_MODE", "raw"),
        ]
        for item in self.patches:
            item.start()
        main_module._CHAT_HISTORY_SESSIONS.clear()
        main_module._UPSTREAM_SESSION_LEASES.clear()

    def tearDown(self):
        main_module._CHAT_HISTORY_SESSIONS.clear()
        main_module._UPSTREAM_SESSION_LEASES.clear()
        for item in reversed(self.patches):
            item.stop()

    async def test_new_sessions_fill_credit_priority_capacity(self):
        bound = [
            main_module._bind_chat_session(
                [{"role": "user", "content": f"request {index}"}],
                {},
                requested_session_id=f"session-{index}",
            )
            for index in range(4)
        ]
        self.assertEqual(
            [item["_account_id"] for item in bound],
            ["rich", "rich", "poor", "poor"],
        )
        self.assertEqual(
            len({item["_account_slot_reservation_id"] for item in bound}),
            4,
        )
        for options in bound:
            self.assertTrue(main_module._cancel_unclaimed_account_slot(options))
        self.assertEqual(auth.account_capacity_snapshot("rich")["inflight"], 0)
        self.assertEqual(auth.account_capacity_snapshot("poor")["inflight"], 0)

    async def test_continuation_stays_on_original_account_when_active_changes(self):
        first = main_module._bind_chat_session(
            [{"role": "user", "content": "first"}],
            {},
            requested_session_id="pinned",
        )
        self.assertEqual(first["_account_id"], "rich")
        self.assertTrue(main_module._cancel_unclaimed_account_slot(first))
        auth._active_account = "poor"

        continuation = main_module._bind_chat_session(
            [
                {"role": "user", "content": "first"},
                {"role": "assistant", "content": "tool call"},
                {"role": "tool", "tool_call_id": "call-1", "content": "done"},
            ],
            {},
            requested_session_id="pinned",
        )
        self.assertEqual(continuation["_account_id"], "rich")
        self.assertTrue(main_module._cancel_unclaimed_account_slot(continuation))

    async def test_dispatch_error_cancels_only_its_unclaimed_reservation(self):
        options = main_module._bind_chat_session(
            [{"role": "user", "content": "bad model"}],
            {},
            requested_session_id="invalid-request",
        )

        class Tracker:
            async def begin(self):
                return None

            async def finish(self, _status):
                return None

        response = main_module._openai_error(
            400, "unsupported", "invalid_request_error"
        )
        with patch.object(
            main_module, "_dispatch_chat", new=AsyncMock(return_value=response)
        ):
            returned = await main_module._tracked_dispatch(
                [], "invalid", options, Tracker()
            )
        self.assertIs(returned, response)
        self.assertEqual(auth.account_capacity_snapshot("rich")["inflight"], 0)

    async def test_bind_failure_cancels_new_reservation(self):
        with patch.object(
            main_module,
            "_ensure_model_account_enabled",
            side_effect=main_module._ModelAccountUnavailable("disabled"),
        ):
            with self.assertRaises(main_module._ModelAccountUnavailable):
                main_module._bind_chat_session(
                    [{"role": "user", "content": "hello"}],
                    {},
                    requested_session_id="bind-error",
                )
        self.assertEqual(auth.account_capacity_snapshot("rich")["inflight"], 0)
