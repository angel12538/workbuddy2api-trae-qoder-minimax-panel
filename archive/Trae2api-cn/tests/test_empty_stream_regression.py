import json
import unittest
from unittest.mock import AsyncMock, patch

from src import main as main_module
from src.sse import EmptyUpstreamResponse


def _frame(delta=None, **fields):
    payload = {"choices": [{"delta": delta or {}, "finish_reason": None}]}
    payload.update(fields)
    return "data: " + json.dumps(payload) + "\n\n"


def _response(*chunks, error=None):
    async def body():
        for chunk in chunks:
            yield chunk
        if error is not None:
            raise error

    return main_module.StreamingResponse(body())


class DeferredEmptyStreamTests(unittest.IsolatedAsyncioTestCase):
    async def consume(self, dispatch, *, mode="remote"):
        options = {"_upstream_mode": mode}
        with (
            patch.object(main_module, "_apply_auto_route", return_value=options),
            patch.object(main_module, "_dispatch_chat", dispatch),
        ):
            return "".join(
                [
                    chunk
                    async for chunk in main_module._deferred_dispatch_stream(
                        [{"role": "user", "content": "hello"}],
                        "glm-5.3",
                        options,
                    )
                ]
            )

    async def test_empty_eof_reports_error(self):
        dispatch = AsyncMock(return_value=_response())
        result = await self.consume(dispatch)
        self.assertIn('"type": "api_error"', result)
        self.assertIn("ended without text", result)
        self.assertEqual(result.count("data: [DONE]"), 1)
        dispatch.assert_awaited_once()

    async def test_done_only_does_not_hide_error_behind_success_sentinel(self):
        dispatch = AsyncMock(return_value=_response("data: [DONE]\n\n"))
        result = await self.consume(dispatch)
        self.assertIn('"error"', result)
        self.assertLess(result.index('"error"'), result.index("data: [DONE]"))
        self.assertEqual(result.count("data: [DONE]"), 1)

    async def test_empty_finish_frame_can_fallback_before_any_output(self):
        finish = 'data: {"choices":[{"delta":{},"finish_reason":"stop"}]}\n\n'
        dispatch = AsyncMock(
            side_effect=[
                _response(_frame({"role": "assistant"}), finish, "data: [DONE]\n\n"),
                _response(_frame({"content": "pong"}), finish, "data: [DONE]\n\n"),
            ]
        )
        result = await self.consume(dispatch, mode="ide")
        self.assertIn("pong", result)
        self.assertNotIn('"error"', result)
        self.assertEqual(result.count("data: [DONE]"), 1)
        self.assertEqual(dispatch.await_count, 2)

    async def test_usage_only_is_error_without_replaying_billed_turn(self):
        dispatch = AsyncMock(
            return_value=_response(
                _frame(usage={"prompt_tokens": 10, "completion_tokens": 1}),
                "data: [DONE]\n\n",
            )
        )
        result = await self.consume(dispatch, mode="ide")
        self.assertIn('"error"', result)
        self.assertEqual(result.count("data: [DONE]"), 1)
        dispatch.assert_awaited_once()

    async def test_whitespace_only_is_error_without_replaying_model_turn(self):
        dispatch = AsyncMock(
            return_value=_response(_frame({"content": " \n"}), "data: [DONE]\n\n")
        )
        result = await self.consume(dispatch, mode="ide")
        self.assertIn('"error"', result)
        dispatch.assert_awaited_once()

    async def test_malformed_data_is_error_without_replaying_unknown_model_turn(self):
        dispatch = AsyncMock(return_value=_response("data: invalid-json\n\n"))
        result = await self.consume(dispatch, mode="ide")
        self.assertIn('"error"', result)
        dispatch.assert_awaited_once()

    async def test_late_transport_failure_after_usage_does_not_fallback(self):
        dispatch = AsyncMock(
            return_value=_response(
                _frame(usage={"prompt_tokens": 10, "completion_tokens": 1}),
                error=RuntimeError("upstream 429 parallel limit"),
            )
        )
        result = await self.consume(dispatch, mode="ide")
        self.assertIn("upstream 429 parallel limit", result)
        self.assertIn('"error"', result)
        dispatch.assert_awaited_once()

    async def test_fragmented_content_frame_remains_valid_output(self):
        content = _frame({"content": "pong"})
        split = content.index("pong") + 2
        dispatch = AsyncMock(
            return_value=_response(content[:split], content[split:], "data: [DONE]\n\n")
        )
        result = await self.consume(dispatch)
        self.assertIn("pong", result)
        self.assertNotIn('"error"', result)

    async def test_reasoning_and_tool_only_responses_remain_valid(self):
        for delta in (
            {"reasoning_content": "I should inspect the file."},
            {"tool_calls": [{"index": 0, "function": {"name": "read_file"}}]},
        ):
            with self.subTest(delta=delta):
                dispatch = AsyncMock(
                    return_value=_response(_frame(delta), "data: [DONE]\n\n")
                )
                result = await self.consume(dispatch)
                self.assertNotIn('"error"', result)

    async def test_legacy_content_eof_still_succeeds(self):
        dispatch = AsyncMock(return_value=_response(_frame({"content": "pong"})))
        result = await self.consume(dispatch)
        self.assertIn("pong", result)
        self.assertNotIn('"error"', result)

    async def test_existing_error_frame_is_forwarded_once(self):
        error = 'data: {"error":{"type":"api_error","message":"upstream failed"}}\n\n'
        dispatch = AsyncMock(return_value=_response(error, "data: [DONE]\n\n"))
        result = await self.consume(dispatch, mode="ide")
        self.assertEqual(result.count("upstream failed"), 1)
        self.assertEqual(result.count("data: [DONE]"), 1)
        dispatch.assert_awaited_once()


class RemoteSafeStreamRetryTests(unittest.IsolatedAsyncioTestCase):
    def mocks(self, remote, *, next_account="account-b", attempts=2):
        from contextlib import ExitStack

        stack = ExitStack()
        stack.enter_context(
            patch.object(main_module.auth, "get_polling_status", return_value={"enabled": True})
        )
        stack.enter_context(
            patch.object(main_module, "_polling_retry_limit", return_value=attempts)
        )
        stack.enter_context(
            patch.object(
                main_module,
                "_next_retry_account_snapshot",
                return_value=(next_account, {"token": "token-" + next_account}),
            )
        )
        stack.enter_context(patch.object(main_module, "_rebind_chat_session_account"))
        stack.enter_context(patch.object(main_module, "run_remote_session", remote))
        return stack

    async def open_response(self):
        return await main_module._run_remote_stream_with_retry(
            [{"role": "user", "content": "hello"}],
            "glm-5.3",
            {"_account_id": "account-a", "_auth_token": "token-account-a"},
        )

    async def test_late_parallel_error_never_replays_public_output(self):
        for delta in (
            {"content": "partial answer"},
            {"reasoning_content": "partial reasoning"},
            {"tool_calls": [{"index": 0, "function": {"name": "read_file"}}]},
        ):
            with self.subTest(delta=delta):
                remote = AsyncMock(
                    return_value=_response(
                        _frame(delta), error=RuntimeError("429 parallel limit after output")
                    )
                )
                with self.mocks(remote):
                    response = await self.open_response()
                    with self.assertRaisesRegex(RuntimeError, "429 parallel limit"):
                        _ = [chunk async for chunk in response.body_iterator]
                remote.assert_awaited_once()

    async def test_usage_or_terminal_frame_blocks_later_rate_limit_retry(self):
        for chunk in (
            _frame(usage={"prompt_tokens": 10, "completion_tokens": 1}),
            "data: [DONE]\n\n",
            'data: {"error":{"message":"upstream failed"}}\n\n',
        ):
            with self.subTest(chunk=chunk):
                remote = AsyncMock(
                    return_value=_response(chunk, error=RuntimeError("429 parallel limit"))
                )
                with self.mocks(remote):
                    response = await self.open_response()
                    with self.assertRaisesRegex(RuntimeError, "429 parallel limit"):
                        _ = [item async for item in response.body_iterator]
                remote.assert_awaited_once()

    async def test_observed_model_error_is_not_replayed_even_if_retryable(self):
        for during_creation in (False, True):
            with self.subTest(during_creation=during_creation):
                error = EmptyUpstreamResponse(
                    "observed model event", retryable=True, observed_model_event=True
                )
                remote = AsyncMock(
                    side_effect=error if during_creation else None,
                    return_value=_response(error=error),
                )
                with self.mocks(remote):
                    with self.assertRaisesRegex(EmptyUpstreamResponse, "observed model event"):
                        response = await self.open_response()
                        _ = [chunk async for chunk in response.body_iterator]
                remote.assert_awaited_once()

    async def test_preoutput_parallel_error_rotates_account(self):
        remote = AsyncMock(
            side_effect=[
                _response(
                    ": keepalive\n\n",
                    _frame({"role": "assistant"}),
                    error=RuntimeError("429 parallel limit"),
                ),
                _response(_frame({"content": "pong"}), "data: [DONE]\n\n"),
            ]
        )
        with self.mocks(remote):
            response = await self.open_response()
            result = "".join([chunk async for chunk in response.body_iterator])
        self.assertIn("pong", result)
        self.assertEqual(remote.await_count, 2)
        self.assertEqual(remote.await_args_list[1].args[3]["_account_id"], "account-b")

    async def test_duplicate_account_rotation_raises_instead_of_silent_eof(self):
        remote = AsyncMock(return_value=_response(error=RuntimeError("429 parallel limit")))
        with self.mocks(remote, next_account="account-a"):
            response = await self.open_response()
            with self.assertRaisesRegex(RuntimeError, "account already attempted"):
                _ = [chunk async for chunk in response.body_iterator]
        remote.assert_awaited_once()

    async def test_exhausted_empty_attempts_preserve_failure(self):
        remote = AsyncMock(
            side_effect=[
                _response(error=EmptyUpstreamResponse("empty account-a", retryable=True)),
                _response(error=EmptyUpstreamResponse("empty account-b", retryable=True)),
            ]
        )
        with self.mocks(remote):
            response = await self.open_response()
            with self.assertRaisesRegex(EmptyUpstreamResponse, "empty account-b"):
                _ = [chunk async for chunk in response.body_iterator]
        self.assertEqual(remote.await_count, 2)

    async def test_empty_noniterator_body_rotates_instead_of_silent_success(self):
        remote = AsyncMock(
            side_effect=[
                main_module.Response(content=b""),
                _response(_frame({"content": "pong"}), "data: [DONE]\n\n"),
            ]
        )
        with self.mocks(remote):
            response = await self.open_response()
            result = "".join([chunk async for chunk in response.body_iterator])
        self.assertIn("pong", result)
        self.assertEqual(remote.await_count, 2)
