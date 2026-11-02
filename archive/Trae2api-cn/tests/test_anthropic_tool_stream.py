"""Exercise tool arguments across native, Chat, and Messages stream layers."""

from __future__ import annotations

import asyncio
import json
import unittest

from src import anthropic_api, sse

TOOLS = [{"type": "function", "function": {"name": "read_file"}}]


class _LineResponse:
    def __init__(self, payloads: list[dict]):
        self.lines = ["data: " + json.dumps(payload) for payload in payloads]
        self.lines.append("data: [DONE]")

    def iter_lines(self):
        return iter(self.lines)


def _native_call(call_id: str, arguments: str, *, index: int = 0) -> dict:
    return {
        "tool_calls": [
            {
                "id": call_id,
                "index": index,
                "function_call": {"name": "read_file", "arguments": arguments},
            }
        ]
    }


def _openai_chunk(tool_calls: list[dict] | None = None, finish_reason=None) -> str:
    body = {
        "choices": [
            {
                "index": 0,
                "delta": {"tool_calls": tool_calls} if tool_calls is not None else {},
                "finish_reason": finish_reason,
            }
        ]
    }
    return "data: " + json.dumps(body) + "\n\n"


def _payload(event: str) -> dict:
    return json.loads(
        next(line[5:].strip() for line in event.splitlines() if line.startswith("data:"))
    )


async def _messages_events(payloads: list[dict], allowed_tools=None) -> list[dict]:
    allowed_tools = allowed_tools or TOOLS
    async def native_stream():
        try:
            async for chunk in sse.translate_ide_stream(
                _LineResponse(payloads),
                "deepseek-v4.1-flash",
                True,
                allowed_tools=allowed_tools,
            ):
                yield chunk
        except sse.InvalidNativeToolArguments as exc:
            # The public dispatcher serializes native stream errors this way.
            yield sse.openai_chunk(
                "chatcmpl-test",
                "deepseek-v4.1-flash",
                {},
                error={"type": "api_error", "message": str(exc)},
            )

    return [
        _payload(event)
        async for event in anthropic_api.translate_chat_stream(
            native_stream(), "deepseek-v4.1-flash"
        )
    ]


async def _chat_events(payloads: list[dict]) -> list[dict]:
    chunks = [
        chunk
        async for chunk in sse.translate_ide_stream(
            _LineResponse(payloads),
            "deepseek-v4.1-flash",
            True,
            allowed_tools=TOOLS,
        )
    ]
    return [_payload(chunk) for chunk in chunks if "[DONE]" not in chunk]


def _chat_tool_inputs(events: list[dict]) -> dict[str, dict]:
    ids: dict[int, str] = {}
    fragments: dict[int, list[str]] = {}
    for event in events:
        for choice in event.get("choices", []):
            for call in choice.get("delta", {}).get("tool_calls", []):
                index = call["index"]
                if call.get("id"):
                    if index in ids and ids[index] != call["id"]:
                        raise ValueError("Distinct calls reused the same public index.")
                    ids[index] = call["id"]
                    fragments.setdefault(index, [])
                fragments[index].append(call.get("function", {}).get("arguments", ""))
    return {
        call_id: json.loads("".join(fragments[index]))
        for index, call_id in ids.items()
    }


def _closed_tool_inputs(events: list[dict]) -> dict[str, dict]:
    blocks: dict[int, dict] = {}
    fragments: dict[int, list[str]] = {}
    result: dict[str, dict] = {}
    for event in events:
        if event["type"] == "content_block_start":
            block = event["content_block"]
            if block["type"] == "tool_use":
                blocks[event["index"]] = block
                fragments[event["index"]] = []
        elif event["type"] == "content_block_delta":
            delta = event["delta"]
            if delta["type"] == "input_json_delta":
                fragments[event["index"]].append(delta["partial_json"])
        elif event["type"] == "content_block_stop" and event["index"] in blocks:
            index = event["index"]
            text = "".join(fragments[index])
            # Strict consumers parse the complete argument stream at stop.
            tool_input = json.loads(text) if text else blocks[index]["input"]
            if not isinstance(tool_input, dict):
                raise ValueError("tool input must be a JSON object")
            result[blocks[index]["id"]] = tool_input
    return result


class AnthropicNativeToolStreamContractTests(unittest.TestCase):
    def test_deepseek_messages_keeps_interleaved_session_tool_json_separate(self):
        payloads = [
            {
                "eventSessionId": "session-a",
                "planItemId": "plan-a",
                "tool_call_info": {
                    "toolCallId": "call-1",
                    "toolCallName": "Read",
                    "parameters": '{"path":"a.txt"',
                },
            },
            {
                "eventSessionId": "session-b",
                "planItemId": "plan-b",
                "tool_call_info": {
                    "toolCallId": "call-1",
                    "toolCallName": "Write",
                    "parameters": '{"path":"b.txt"}',
                },
            },
            {
                "eventSessionId": "session-a",
                "planItemId": "plan-a",
                "tool_call_info": {
                    "toolCallId": "call-1",
                    "toolCallName": "Read",
                    "parameters": "}",
                },
            },
            {"finish_reason": "tool_calls"},
        ]
        tools = [
            {"type": "function", "function": {"name": "Read"}},
            {"type": "function", "function": {"name": "Write"}},
        ]

        events = asyncio.run(_messages_events(payloads, allowed_tools=tools))
        inputs = _closed_tool_inputs(events)

        self.assertEqual(
            sorted(inputs.values(), key=lambda value: value["path"]),
            [{"path": "a.txt"}, {"path": "b.txt"}],
        )
        self.assertEqual(len(inputs), 2)
        self.assertEqual(events[-1]["type"], "message_stop")

    def test_repaired_native_arguments_are_repaired_on_messages_wire(self):
        payloads = [_native_call("call_repaired", '{"path":"README.md"')]
        completion = asyncio.run(
            sse.collect_nonstream_ide(
                _LineResponse(payloads), "deepseek-v4-flash", allowed_tools=TOOLS
            )
        )
        arguments = completion["choices"][0]["message"]["tool_calls"][0]["function"][
            "arguments"
        ]
        self.assertEqual(json.loads(arguments), {"path": "README.md"})

        events = asyncio.run(_messages_events(payloads))

        self.assertEqual(
            _closed_tool_inputs(events), {"call_repaired": {"path": "README.md"}}
        )
        self.assertEqual(events[-1]["type"], "message_stop")

    def test_nested_and_string_truncation_are_repaired_on_messages_wire(self):
        cases = [
            ('{"nested":{"paths":["a.txt","b.txt"]', {"nested": {"paths": ["a.txt", "b.txt"]}}),
            ('{"path":"README.md', {"path": "README.md"}),
        ]
        for arguments, expected in cases:
            with self.subTest(arguments=arguments):
                events = asyncio.run(
                    _messages_events([_native_call("call_repaired", arguments)])
                )
                self.assertEqual(
                    _closed_tool_inputs(events), {"call_repaired": expected}
                )

    def test_repaired_arguments_are_present_on_chat_completions_wire(self):
        cases = [
            ('{"path":"README.md"', {"path": "README.md"}),
            ('{"nested":{"paths":["a.txt","b.txt"]', {"nested": {"paths": ["a.txt", "b.txt"]}}),
            ('{"path":"README.md', {"path": "README.md"}),
        ]
        for arguments, expected in cases:
            with self.subTest(arguments=arguments):
                events = asyncio.run(
                    _chat_events([_native_call("call_repaired", arguments)])
                )
                self.assertEqual(
                    _chat_tool_inputs(events), {"call_repaired": expected}
                )

    def test_empty_native_arguments_are_json_object_on_both_wires(self):
        payloads = [_native_call("call_empty", "")]
        chat_events = asyncio.run(_chat_events(payloads))
        messages_events = asyncio.run(_messages_events(payloads))

        self.assertEqual(_chat_tool_inputs(chat_events), {"call_empty": {}})
        self.assertEqual(_closed_tool_inputs(messages_events), {"call_empty": {}})

    def test_distinct_native_ids_reusing_source_index_do_not_merge_messages_json(self):
        events = asyncio.run(
            _messages_events(
                [
                    _native_call("call_a", '{"path":"a.txt"}'),
                    _native_call("call_b", '{"path":"b.txt"}'),
                ]
            )
        )

        self.assertEqual(
            _closed_tool_inputs(events),
            {"call_a": {"path": "a.txt"}, "call_b": {"path": "b.txt"}},
        )
        self.assertEqual(
            [
                event["delta"]["stop_reason"]
                for event in events
                if event["type"] == "message_delta"
            ],
            ["tool_use"],
        )

    def test_distinct_native_ids_reusing_index_have_unique_public_chat_indexes(self):
        events = asyncio.run(
            _chat_events(
                [
                    _native_call("call_a", '{"path":"a.txt"}'),
                    _native_call("call_b", '{"path":"b.txt"}'),
                ]
            )
        )

        self.assertEqual(
            _chat_tool_inputs(events),
            {"call_a": {"path": "a.txt"}, "call_b": {"path": "b.txt"}},
        )

    def test_unrepairable_native_arguments_do_not_close_invalid_messages_block(self):
        events = asyncio.run(
            _messages_events([_native_call("call_invalid", '{"path":')])
        )

        self.assertTrue(
            any(event["type"] == "error" for event in events),
            "An invalid streamed tool must report an error before block completion.",
        )
        self.assertNotIn("message_stop", [event["type"] for event in events])
        self.assertNotIn(
            "content_block_stop", [event["type"] for event in events]
        )

    def test_independent_indexed_native_calls_keep_interleaved_arguments_isolated(self):
        events = asyncio.run(
            _messages_events(
                [
                    _native_call("call_a", '{"path":', index=0),
                    _native_call("call_b", '{"path":', index=1),
                    {
                        "tool_calls": [
                            {
                                "index": 1,
                                "function_call": {"arguments": '"b.txt"}'},
                            }
                        ]
                    },
                    {
                        "tool_calls": [
                            {
                                "index": 0,
                                "function_call": {"arguments": '"a.txt"}'},
                            }
                        ]
                    },
                ]
            )
        )

        self.assertEqual(
            _closed_tool_inputs(events),
            {"call_a": {"path": "a.txt"}, "call_b": {"path": "b.txt"}},
        )

    def test_replayed_complete_native_call_does_not_duplicate_arguments(self):
        payloads = [
            _native_call("call_replay", '{"path":"a.txt"}'),
            _native_call("call_replay", '{"path":"a.txt"}'),
        ]
        chat_events = asyncio.run(_chat_events(payloads))
        messages_events = asyncio.run(_messages_events(payloads))

        self.assertEqual(
            _chat_tool_inputs(chat_events), {"call_replay": {"path": "a.txt"}}
        )
        self.assertEqual(
            _closed_tool_inputs(messages_events),
            {"call_replay": {"path": "a.txt"}},
        )


class AnthropicTranslatorToolStreamTests(unittest.TestCase):
    def test_reused_source_index_with_distinct_ids_gets_distinct_blocks(self):
        async def stream():
            yield _openai_chunk(
                [
                    {
                        "index": 0,
                        "id": "call_a",
                        "function": {
                            "name": "read_file",
                            "arguments": '{"path":"a.txt"}',
                        },
                    }
                ]
            )
            yield _openai_chunk(
                [
                    {
                        "index": 0,
                        "id": "call_b",
                        "function": {
                            "name": "read_file",
                            "arguments": '{"path":"b.txt"}',
                        },
                    }
                ]
            )
            yield _openai_chunk(finish_reason="tool_calls")
            yield "data: [DONE]\n\n"

        events = asyncio.run(
            _collect_payloads(anthropic_api.translate_chat_stream(stream(), "deepseek"))
        )

        self.assertEqual(
            _closed_tool_inputs(events),
            {"call_a": {"path": "a.txt"}, "call_b": {"path": "b.txt"}},
        )

    def test_missing_indexes_use_call_position_without_cross_streaming(self):
        async def stream():
            yield _openai_chunk(
                [
                    {
                        "id": "call_a",
                        "function": {"name": "read_file", "arguments": '{"path":'},
                    },
                    {
                        "id": "call_b",
                        "function": {"name": "read_file", "arguments": '{"path":'},
                    },
                ]
            )
            yield _openai_chunk(
                [
                    {"function": {"arguments": '"a.txt"}'}},
                    {"function": {"arguments": '"b.txt"}'}},
                ]
            )
            yield _openai_chunk(finish_reason="tool_calls")
            yield "data: [DONE]\n\n"

        events = asyncio.run(
            _collect_payloads(anthropic_api.translate_chat_stream(stream(), "deepseek"))
        )

        self.assertEqual(
            _closed_tool_inputs(events),
            {"call_a": {"path": "a.txt"}, "call_b": {"path": "b.txt"}},
        )

    def test_empty_arguments_use_content_block_start_empty_object(self):
        async def stream():
            yield _openai_chunk(
                [
                    {
                        "index": 0,
                        "id": "call_empty",
                        "function": {"name": "read_file", "arguments": ""},
                    }
                ]
            )
            yield _openai_chunk(finish_reason="tool_calls")
            yield "data: [DONE]\n\n"

        events = asyncio.run(
            _collect_payloads(anthropic_api.translate_chat_stream(stream(), "deepseek"))
        )

        self.assertEqual(_closed_tool_inputs(events), {"call_empty": {}})

    def test_short_repeated_true_deltas_are_preserved_verbatim(self):
        async def stream():
            yield _openai_chunk(
                [
                    {
                        "index": 0,
                        "id": "call_braces",
                        "function": {"name": "read_file", "arguments": '{"path":"'},
                    }
                ]
            )
            for fragment in ("{", "{", "}}", "}}", '"}'):
                yield _openai_chunk(
                    [{"index": 0, "function": {"arguments": fragment}}]
                )
            yield _openai_chunk(finish_reason="tool_calls")
            yield "data: [DONE]\n\n"

        events = asyncio.run(
            _collect_payloads(anthropic_api.translate_chat_stream(stream(), "deepseek"))
        )

        self.assertEqual(
            _closed_tool_inputs(events), {"call_braces": {"path": "{{}}}}"}}
        )

    def test_invalid_arguments_end_with_error_not_successful_envelope(self):
        async def stream():
            yield _openai_chunk(
                [
                    {
                        "index": 0,
                        "id": "call_bad",
                        "function": {
                            "name": "read_file",
                            "arguments": '{"path":',
                        },
                    }
                ]
            )
            yield _openai_chunk(finish_reason="tool_calls")
            yield "data: [DONE]\n\n"

        events = asyncio.run(
            _collect_payloads(anthropic_api.translate_chat_stream(stream(), "deepseek"))
        )
        event_types = [event["type"] for event in events]

        self.assertEqual(event_types[-1], "error")
        self.assertNotIn("content_block_stop", event_types)
        self.assertNotIn("message_stop", event_types)
        self.assertIn("invalid JSON", events[-1]["error"]["message"])

    def test_cumulative_argument_snapshots_are_not_concatenated(self):
        async def stream():
            yield _openai_chunk(
                [
                    {
                        "index": 0,
                        "id": "call_snapshot",
                        "function": {
                            "name": "read_file",
                            "arguments": '{"path":"',
                        },
                    }
                ]
            )
            yield _openai_chunk(
                [
                    {
                        "index": 0,
                        "function": {
                            "arguments": '{"path":"README.md"}',
                        },
                    }
                ]
            )
            yield _openai_chunk(finish_reason="tool_calls")
            yield "data: [DONE]\n\n"

        events = asyncio.run(
            _collect_payloads(anthropic_api.translate_chat_stream(stream(), "deepseek"))
        )

        self.assertEqual(
            _closed_tool_inputs(events), {"call_snapshot": {"path": "README.md"}}
        )

    def test_whitespace_changed_cumulative_snapshot_is_replaced(self):
        async def stream():
            yield _openai_chunk(
                [
                    {
                        "index": 0,
                        "id": "call_snapshot_ws",
                        "function": {
                            "name": "read_file",
                            "arguments": '{"path":"',
                        },
                    }
                ]
            )
            # The second frame is a cumulative snapshot with formatting that
            # does not share the first frame's literal prefix.
            yield _openai_chunk(
                [
                    {
                        "index": 0,
                        "function": {
                            "arguments": '{ "path" : "README.md" }',
                        },
                    }
                ]
            )
            yield _openai_chunk(finish_reason="tool_calls")
            yield "data: [DONE]\n\n"

        events = asyncio.run(
            _collect_payloads(
                anthropic_api.translate_chat_stream(stream(), "deepseek")
            )
        )
        self.assertEqual(
            _closed_tool_inputs(events),
            {"call_snapshot_ws": {"path": "README.md"}},
        )

    def test_truncated_json_is_closed_before_messages_block_stop(self):
        async def stream():
            yield _openai_chunk(
                [
                    {
                        "index": 0,
                        "id": "call_truncated",
                        "function": {
                            "name": "read_file",
                            "arguments": '{"path":"README.md"',
                        },
                    }
                ]
            )
            yield _openai_chunk(finish_reason="tool_calls")
            yield "data: [DONE]\n\n"

        events = asyncio.run(
            _collect_payloads(anthropic_api.translate_chat_stream(stream(), "deepseek"))
        )

        self.assertEqual(
            _closed_tool_inputs(events),
            {"call_truncated": {"path": "README.md"}},
        )
        self.assertEqual(events[-1]["type"], "message_stop")

    def test_nonstream_invalid_arguments_are_not_hidden_in_raw_wrapper(self):
        completion = {
            "choices": [
                {
                    "message": {
                        "tool_calls": [
                            {
                                "id": "call_bad",
                                "function": {
                                    "name": "read_file",
                                    "arguments": '{"path":',
                                },
                            }
                        ]
                    },
                    "finish_reason": "tool_calls",
                }
            ]
        }

        with self.assertRaisesRegex(
            anthropic_api.AnthropicRequestError, "invalid JSON"
        ):
            anthropic_api.completion_to_message(completion, "deepseek")

    def test_parallel_tool_policy_maps_anthropic_and_compat_flags(self):
        base = {
            "messages": [{"role": "user", "content": "read files"}],
            "tools": [
                {
                    "name": "read_file",
                    "input_schema": {"type": "object", "properties": {}},
                }
            ],
        }
        cases = [
            (
                {"tool_choice": {"type": "auto", "disable_parallel_tool_use": True}},
                False,
            ),
            (
                {"tool_choice": {"type": "auto", "disable_parallel_tool_use": False}},
                True,
            ),
            (
                {
                    "tool_choice": {
                        "type": "auto",
                        "disable_parallel_tool_use": False,
                    },
                    "parallel_tool_calls": False,
                },
                False,
            ),
            (
                {
                    "tool_choice": {
                        "type": "auto",
                        "disable_parallel_tool_use": True,
                    },
                    "parallel_tool_calls": True,
                },
                False,
            ),
            ({"parallel_tool_calls": True}, True),
        ]
        for extra, expected in cases:
            with self.subTest(extra=extra):
                _, options = anthropic_api.normalize_request({**base, **extra})
                self.assertIs(options["parallel_tool_calls"], expected)


class AnthropicEmptyResponseTests(unittest.TestCase):
    def test_nonstream_empty_completion_is_rejected(self):
        messages = [
            {},
            {"content": None},
            {"content": ""},
            {"content": " \n\t "},
            {"content": [], "reasoning_content": " \n "},
            {"content": None, "tool_calls": [None]},
        ]
        for message in messages:
            with self.subTest(message=message):
                completion = {
                    "choices": [{"message": message, "finish_reason": "stop"}],
                    "usage": {"prompt_tokens": 12, "completion_tokens": 3},
                }
                with self.assertRaisesRegex(
                    anthropic_api.AnthropicRequestError, "empty response"
                ):
                    anthropic_api.completion_to_message(completion, "deepseek")

    def test_nonstream_requested_reasoning_and_empty_tool_input_remain_valid(self):
        for message, expected_type in (
            ({"reasoning_content": "Inspecting the request."}, "thinking"),
            (
                {
                    "tool_calls": [
                        {"function": {"name": "read_file", "arguments": "{}"}}
                    ]
                },
                "tool_use",
            ),
        ):
            with self.subTest(expected_type=expected_type):
                result = anthropic_api.completion_to_message(
                    {"choices": [{"message": message}]}, "deepseek"
                )
                self.assertEqual(result["content"][0]["type"], expected_type)

    def test_empty_streams_never_emit_successful_terminal(self):
        usage_only = "data: " + json.dumps(
            {"choices": [], "usage": {"completion_tokens": 3}}
        ) + "\n\n"
        cases = [
            [],
            ["data: [DONE]\n\n"],
            [_openai_chunk(finish_reason="stop"), "data: [DONE]\n\n"],
            [_openai_chunk(finish_reason="stop")],
            [_openai_chunk(finish_reason="stop").rstrip()],
            [usage_only, "data: [DONE]\n\n"],
        ]
        for frames in cases:
            with self.subTest(frames=frames):
                async def stream():
                    for frame in frames:
                        yield frame

                events = asyncio.run(
                    _collect_payloads(
                        anthropic_api.translate_chat_stream(stream(), "deepseek")
                    )
                )
                self.assertEqual(events[-1]["type"], "error")
                self.assertIn("empty response", events[-1]["error"]["message"])
                self.assertNotIn("message_delta", [event["type"] for event in events])
                self.assertNotIn("message_stop", [event["type"] for event in events])

    def test_whitespace_streams_never_emit_successful_terminal(self):
        payloads = [
            {"choices": [{"delta": {"content": " \n\t "}}]},
            {"choices": [{"delta": {"reasoning_content": " \n "}}]},
            {"reasoning_content": " \n "},
        ]
        for payload in payloads:
            for terminal in ("data: [DONE]\n\n", _openai_chunk(finish_reason="stop")):
                with self.subTest(payload=payload, terminal=terminal):
                    async def stream():
                        yield "data: " + json.dumps(payload) + "\n\n"
                        yield terminal

                    events = asyncio.run(
                        _collect_payloads(
                            anthropic_api.translate_chat_stream(stream(), "deepseek")
                        )
                    )
                    self.assertEqual(events[-1]["type"], "error")
                    self.assertNotIn("message_stop", [event["type"] for event in events])

    def test_reasoning_stream_remains_valid(self):
        async def stream():
            yield "data: " + json.dumps(
                {"choices": [{"delta": {"reasoning_content": "Inspecting."}}]}
            ) + "\n\n"
            yield "data: [DONE]\n\n"

        events = asyncio.run(
            _collect_payloads(anthropic_api.translate_chat_stream(stream(), "deepseek"))
        )
        self.assertEqual(events[-1]["type"], "message_stop")
        self.assertIn(
            "thinking_delta",
            [event.get("delta", {}).get("type") for event in events],
        )


async def _collect_payloads(events) -> list[dict]:
    return [_payload(event) async for event in events]


if __name__ == "__main__":
    unittest.main()
