"""Session history hashing stays compatible without repeated prefix encoding."""

from __future__ import annotations

import json
import time
import unittest
from contextlib import ExitStack
from unittest.mock import patch

from src import main as main_module


class _StringFallback:
    def __str__(self):
        return "custom fallback"


class SessionHistoryHashingTests(unittest.TestCase):
    def setUp(self):
        main_module._CHAT_HISTORY_SESSIONS.clear()
        main_module._UPSTREAM_SESSION_LEASES.clear()
        self.addCleanup(main_module._CHAT_HISTORY_SESSIONS.clear)
        self.addCleanup(main_module._UPSTREAM_SESSION_LEASES.clear)

    def _account(self, account_id="account-a"):
        stack = ExitStack()
        self.addCleanup(stack.close)
        stack.enter_context(patch.object(main_module, "UPSTREAM_MODE", "cli"))
        stack.enter_context(
            patch.object(main_module.auth, "get_polling_status", return_value={"enabled": False})
        )
        stack.enter_context(
            patch.object(
                main_module.auth,
                "get_active_account_snapshot",
                return_value=(account_id, {"token": "synthetic-token"}),
            )
        )
        stack.enter_context(patch.object(main_module, "_ensure_model_account_enabled"))

    def _remember(self, messages, session_id, account_id="account-a"):
        now = time.monotonic()
        main_module._CHAT_HISTORY_SESSIONS[main_module._chat_history_key(messages)] = (
            session_id, now
        )
        main_module._UPSTREAM_SESSION_LEASES[session_id] = main_module._UpstreamSessionLease(
            account_id=account_id,
            billing_id=account_id,
            auth_token="synthetic-token",
            last_client_activity=now,
        )

    def test_prefix_keys_match_canonical_hashes(self):
        messages = [
            {"role": "user", "content": "\u4e2d\u6587\U0001f642\n\\\""},
            {
                "role": "assistant",
                "content": None,
                "tool_calls": [
                    {
                        "function": {"arguments": '{"z": 1, "a": "value"}', "name": "read"},
                        "id": "call-1",
                    }
                ],
            },
            {
                "role": "tool",
                "content": [
                    {"text": "result", "type": "text"},
                    {"image_url": {"url": "data:image/png;base64,AAAA"}, "type": "image_url"},
                ],
                "nested": {"z": [True, False, None, 1, 2.5], "a": _StringFallback()},
            },
        ]
        expected = [
            main_module._chat_history_key(messages, length)
            for length in range(1, len(messages) + 1)
        ]
        self.assertEqual(main_module._chat_history_prefix_keys(messages), expected)
        self.assertEqual(main_module._chat_history_prefix_keys([]), [])

    def test_binding_prefers_longest_matching_prefix(self):
        self._account()
        messages = [
            {"role": "user", "content": "first"},
            {"role": "assistant", "content": "answer"},
            {"role": "user", "content": "continue"},
        ]
        self._remember(messages[:1], "short-session")
        self._remember(messages[:2], "long-session")

        bound = main_module._bind_chat_session(messages, {})

        self.assertEqual(bound["session_id"], "long-session")
        self.assertEqual(
            main_module._CHAT_HISTORY_SESSIONS[main_module._chat_history_key(messages)][0],
            "long-session",
        )

    def test_binding_skips_longer_prefix_from_different_account(self):
        self._account()
        messages = [
            {"role": "user", "content": "first"},
            {"role": "assistant", "content": "answer"},
            {"role": "user", "content": "continue"},
        ]
        self._remember(messages[:1], "same-account")
        self._remember(messages[:2], "other-account", "account-b")

        bound = main_module._bind_chat_session(messages, {})

        self.assertEqual(bound["session_id"], "same-account")
        self.assertEqual(bound["_account_id"], "account-a")

    def test_explicit_session_stays_pinned_to_original_account(self):
        self._account("account-b")
        messages = [{"role": "user", "content": "first"}]
        self._remember(messages, "pinned-session", "account-a")

        bound = main_module._bind_chat_session(
            [*messages, {"role": "assistant", "content": "answer"}],
            {},
            requested_session_id="pinned-session",
        )

        self.assertEqual(bound["session_id"], "pinned-session")
        self.assertEqual(bound["_account_id"], "account-a")

    def test_binding_serializes_each_message_once_outside_session_lock(self):
        self._account()
        messages = [
            {"role": "assistant" if index % 2 else "user", "content": "x" * 5000}
            for index in range(600)
        ]
        original_dumps = json.dumps
        serialized_messages = []
        lock_held = False

        class SessionLock:
            def __enter__(self):
                nonlocal lock_held
                lock_held = True

            def __exit__(self, *_args):
                nonlocal lock_held
                lock_held = False

        def encode_message(value, *args, **kwargs):
            self.assertFalse(lock_held)
            serialized_messages.append(value)
            return original_dumps(value, *args, **kwargs)

        with (
            patch.object(main_module, "_CHAT_SESSION_LOCK", SessionLock()),
            patch.object(main_module.json, "dumps", side_effect=encode_message),
        ):
            main_module._bind_chat_session(messages, {})

        self.assertEqual(len(serialized_messages), len(messages))
        for encoded, message in zip(serialized_messages, messages):
            self.assertIs(encoded, message)


if __name__ == "__main__":
    unittest.main()
