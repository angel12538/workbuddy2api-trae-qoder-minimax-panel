import json
import unittest
from contextlib import ExitStack
from unittest.mock import AsyncMock, patch

from src import main as main_module
from src import sse


async def _events(*events):
    for event in events:
        yield event


class RemoteNonstreamRetryGuardTests(unittest.IsolatedAsyncioTestCase):
    def _retry_mocks(self, remote):
        stack = ExitStack()
        stack.enter_context(
            patch.object(main_module.auth, "get_polling_status", return_value={"enabled": True})
        )
        stack.enter_context(
            patch.object(main_module, "_polling_retry_limit", return_value=2)
        )
        rotate = stack.enter_context(
            patch.object(
                main_module,
                "_next_retry_account_snapshot",
                return_value=("account-b", {"token": "token-b"}),
            )
        )
        stack.enter_context(patch.object(main_module, "_account_id_from_token", return_value=""))
        stack.enter_context(patch.object(main_module, "_rebind_chat_session_account"))
        stack.enter_context(patch.object(main_module, "run_remote_session", remote))
        return stack, rotate

    async def _request(self):
        return await main_module._run_remote_with_retry(
            [{"role": "user", "content": "hello"}],
            "glm-5.3",
            False,
            {"_account_id": "account-a", "_auth_token": "token-a"},
        )

    async def test_preoutput_capacity_preserves_error_and_rotates_account(self):
        for payload in (
            {"code": 429, "message": "busy"},
            {"error": {"errorCode": "3004", "detail": "busy"}},
            {"message": "parallel limit"},
        ):
            with self.subTest(payload=payload):
                calls = []
                failures = []

                async def remote(_messages, _model, _stream, options):
                    calls.append(options["_account_id"])
                    if len(calls) > 1:
                        return "ok"
                    try:
                        return await sse.collect_nonstream_web(
                            _events(("error", payload)), "glm-5.3", fail_on_empty=True
                        )
                    except sse.UpstreamEventError as exc:
                        failures.append(exc)
                        raise

                stack, rotate = self._retry_mocks(remote)
                with stack:
                    self.assertEqual(await self._request(), "ok")
                self.assertEqual(calls, ["account-a", "account-b"])
                rotate.assert_called_once()
                self.assertEqual(failures[0].code, sse._upstream_error_details(payload)[0])
                self.assertTrue(failures[0].retryable)
                self.assertTrue(failures[0].account_quota_exhausted)
                self.assertFalse(failures[0].observed_model_event)

    async def test_model_output_before_capacity_error_never_rotates(self):
        tool_block = (
            '<opencode_tool_call>{"id":"call_read","name":"read_file",'
            '"input":{"path":"README.md"}}</opencode_tool_call>'
        )
        for name, before, error in (
            ("text", ("message", {"content": "partial answer"}), {}),
            ("plan content", ("plan_item", {"id": "p1", "content": "partial answer"}), {}),
            (
                "hidden reasoning",
                ("plan_item", {"id": "p1", "thought": "", "reasoning_content": "private trace"}),
                {},
            ),
            ("tool", ("message", {"content": tool_block}), {}),
            ("usage", ("token_usage", {"prompt_tokens": 10, "completion_tokens": 1}), {}),
            ("error usage", ("heartbeat", {}), {"usage": {"prompt_tokens": 10}}),
            (
                "finish summary",
                ("plan_item", {"tool_call_info": {"name": "finish", "params": {"summary": "answer"}}}),
                {},
            ),
        ):
            with self.subTest(name=name):
                calls = []

                async def remote(_messages, _model, _stream, options):
                    calls.append(options["_account_id"])
                    return await sse.collect_nonstream_web(
                        _events(before, ("error", {"code": 429, "message": "parallel limit", **error})),
                        "glm-5.3",
                        fail_on_empty=True,
                    )

                stack, rotate = self._retry_mocks(remote)
                with stack, self.assertRaises(sse.UpstreamEventError) as raised:
                    await self._request()
                self.assertEqual(calls, ["account-a"])
                self.assertEqual(raised.exception.code, 429)
                self.assertTrue(raised.exception.observed_model_event)
                self.assertFalse(raised.exception.retryable)
                rotate.assert_not_called()

    async def test_observed_or_usage_error_cannot_override_retry_guard(self):
        for error_type in (RuntimeError, sse.EmptyUpstreamResponse):
            for evidence in (
                {"observed_model_event": True},
                {"usage": {"prompt_tokens": 10}},
            ):
                with self.subTest(error_type=error_type, evidence=evidence):
                    error = error_type("429 parallel limit")
                    for name, value in evidence.items():
                        setattr(error, name, value)
                    remote = AsyncMock(side_effect=error)
                    stack, rotate = self._retry_mocks(remote)
                    with stack, self.assertRaises(error_type):
                        await self._request()
                    remote.assert_awaited_once()
                    rotate.assert_not_called()


class RemoteNonstreamWorkFallbackGuardTests(unittest.IsolatedAsyncioTestCase):
    def _session_mocks(self, *, stream_events=None, collector=None, create_error=None, use_transport=False):
        stack = ExitStack()
        stack.enter_context(patch.dict(main_module.os.environ, {"TRAE_REMOTE_WORK_FALLBACK": "1"}))
        stack.enter_context(
            patch.object(main_module.auth, "get_account_record", return_value={"token": "token-a"})
        )
        stack.enter_context(patch.object(main_module.auth, "is_account_model_enabled", return_value=True))
        stack.enter_context(patch.object(main_module, "_account_id_from_token", return_value=""))
        stack.enter_context(patch.object(main_module.trae_client, "acquire_web_slot", new=AsyncMock()))
        stack.enter_context(patch.object(main_module.trae_client, "release_web_slot"))
        stack.enter_context(patch.object(main_module.httpx, "AsyncClient", return_value=AsyncMock()))
        create = stack.enter_context(
            patch.object(
                main_module.trae_remote_client,
                "create_session",
                new=AsyncMock(side_effect=create_error) if create_error else AsyncMock(return_value=("s1", "m1")),
            )
        )
        stack.enter_context(
            patch.object(
                main_module.trae_remote_client,
                "_stream_events_unbounded" if use_transport else "stream_events",
                side_effect=stream_events or (lambda *a, **kw: _events()),
            )
        )
        stop = stack.enter_context(patch.object(main_module.trae_remote_client, "stop_session", new=AsyncMock()))
        stack.enter_context(patch.object(main_module, "_bind_usage_turn"))
        stack.enter_context(patch.object(main_module, "_track_usage_from_result"))
        stack.enter_context(patch.object(main_module, "_track_usage_from_exception"))
        if collector is not None:
            stack.enter_context(patch.object(main_module, "collect_nonstream_web", collector))
        return stack, create, stop

    async def _request(self):
        return await main_module.run_remote_session(
            [{"role": "user", "content": "hello"}],
            "glm-5.3",
            False,
            {"_account_id": "account-a", "_auth_token": "token-a"},
        )

    async def test_capacity_event_does_not_fallback_to_same_account_work(self):
        for payload in ({"code": 429, "message": "busy"}, {"message": "parallel limit"}):
            with self.subTest(payload=payload):
                stack, create, stop = self._session_mocks(
                    stream_events=lambda *a, **kw: _events(("error", payload))
                )
                with stack, self.assertRaises(sse.UpstreamEventError):
                    await self._request()
                create.assert_awaited_once()
                stop.assert_awaited_once()

    async def test_capacity_create_failure_does_not_fallback_to_work(self):
        for message in ("Trae remote create_session [429]: busy", "parallel limit"):
            with self.subTest(message=message):
                stack, create, _ = self._session_mocks(create_error=RuntimeError(message))
                with stack, self.assertRaisesRegex(RuntimeError, "busy|parallel"):
                    await self._request()
                create.assert_awaited_once()

    async def test_real_first_capacity_error_rotates_without_same_account_work(self):
        for payload in (
            {"code": 429, "message": "busy"},
            {"error": {"code": 3004, "message": "busy"}},
            {"message": "parallel limit"},
        ):
            with self.subTest(payload=payload):
                def events(_client, token, *_args, **kwargs):
                    if token == "token-a":
                        return _events(("error", payload))
                    return _events(("message", {"content": "account b answer"}), ("done", {}))

                stack, create, _ = self._session_mocks(stream_events=events, use_transport=True)
                with (
                    stack,
                    patch.object(main_module.auth, "get_polling_status", return_value={"enabled": True}),
                    patch.object(main_module, "_polling_retry_limit", return_value=2),
                    patch.object(
                        main_module,
                        "_next_retry_account_snapshot",
                        return_value=("account-b", {"token": "token-b"}),
                    ),
                    patch.object(main_module, "_rebind_chat_session_account"),
                ):
                    response = await main_module._run_remote_with_retry(
                        [{"role": "user", "content": "hello"}],
                        "glm-5.3",
                        False,
                        {"_account_id": "account-a", "_auth_token": "token-a"},
                    )
                self.assertEqual(json.loads(response.body)["choices"][0]["message"]["content"], "account b answer")
                self.assertEqual([call.args[1] for call in create.await_args_list], ["token-a", "token-b"])
                self.assertTrue(all("_remote_agent_type" not in call.kwargs["options"] for call in create.await_args_list))

    async def test_observed_or_usage_error_does_not_fallback_even_if_retryable(self):
        for evidence in ({"observed_model_event": True}, {"usage": {"prompt_tokens": 10}}):
            with self.subTest(evidence=evidence):
                collector = AsyncMock(side_effect=sse.EmptyUpstreamResponse("busy", **evidence))
                stack, create, _ = self._session_mocks(collector=collector)
                with stack, self.assertRaises(sse.EmptyUpstreamResponse):
                    await self._request()
                create.assert_awaited_once()
                collector.assert_awaited_once()

    async def test_streaming_session_blocks_observed_or_usage_fallback(self):
        for evidence in ({"observed_model_event": True}, {"usage": {"prompt_tokens": 10}}):
            with self.subTest(evidence=evidence):
                error = sse.EmptyUpstreamResponse("observed turn", retryable=True, **evidence)

                async def translate(*args, **kwargs):
                    if False:
                        yield ""
                    raise error

                stack, create, _ = self._session_mocks()
                with (
                    stack,
                    patch.object(main_module, "translate_web_events", translate),
                    patch.object(main_module.auth, "get_polling_status", return_value={"enabled": False}),
                ):
                    response = await main_module.run_remote_session(
                        [{"role": "user", "content": "hello"}],
                        "glm-5.3",
                        True,
                        {"_account_id": "account-a", "_auth_token": "token-a"},
                    )
                    with self.assertRaises(sse.EmptyUpstreamResponse):
                        _ = [chunk async for chunk in response.body_iterator]
                create.assert_awaited_once()

    async def test_genuinely_empty_agent_retains_work_fallback(self):
        def events(*args, options=None, **kwargs):
            if (options or {}).get("_remote_agent_type") == "solo_work_remote":
                return _events(("message", {"content": "work answer"}), ("done", {}))
            return _events()

        stack, create, _ = self._session_mocks(stream_events=events)
        with stack:
            response = await self._request()
        self.assertEqual(json.loads(response.body)["choices"][0]["message"]["content"], "work answer")
        self.assertEqual(create.await_count, 2)
        self.assertEqual(create.await_args_list[1].kwargs["options"]["_remote_agent_type"], "solo_work_remote")
