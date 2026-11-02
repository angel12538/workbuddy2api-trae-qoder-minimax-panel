"""Coverage for the four ZedeX-inspired additions:

* queue telemetry must not reach assistant text
* truncation detection / auto-continuation helpers
* image input is forwarded instead of dropped
* the Anthropic /v1/messages ingress

All tests are offline: no upstream Trae request is made.
"""

from __future__ import annotations

import asyncio
import json
import os
import unittest
from unittest import mock

from src import (
    anthropic_api,
    cli_client,
    continuation,
    images,
    raw_client,
    sse,
    trae_client,
)

class QueueTelemetryTests(unittest.TestCase):
    """Queue position remains part of the raw assistant stream contract."""

    def test_queue_position_is_assistant_text(self):
        response = _LineResponse(
            [
                'data: {"event":"request_wait_in_queue","position":7}',
                'data: {"response":"the answer"}',
                'data: {"finish_reason":"stop"}',
                "data: [DONE]",
            ]
        )
        chunks = asyncio.run(_collect(sse.translate_ide_stream(response, "m", True)))
        parsed, done = _parse_chunks(chunks)
        content = "".join(
            choice.get("delta", {}).get("content", "")
            for event in parsed
            for choice in event.get("choices", [])
        )
        self.assertEqual(content, "排队中，当前位置：7\nthe answer")
        self.assertNotIn(": relay-keepalive\n\n", chunks)
        self.assertTrue(done)

    def test_queue_position_reaches_upstream_metadata(self):
        response = _LineResponse(
            [
                'data: {"event":"request_wait_in_queue","position":12}',
                'data: {"response":"done"}',
                'data: {"finish_reason":"stop"}',
                "data: [DONE]",
            ]
        )
        metadata: dict = {}
        asyncio.run(
            _collect(
                sse.translate_ide_stream(
                    response, "m", True, upstream_metadata=metadata
                )
            )
        )
        self.assertEqual(metadata.get("queue_position"), 12)

class ContinuationTests(unittest.TestCase):
    def test_truncated_answers_are_detected(self):
        fence = "explain:\n" + "```" + "python\nprint(1)"
        cases = {
            "unclosed code fence": fence,
            "trailing comma": "first, second,",
            "trailing pipe": "| a | b |",
            "trailing ellipsis": "and then...",
            "unclosed bracket": "def f(): return [1, 2",
            "trailing colon": "Steps:",
        }
        for label, text in cases.items():
            with self.subTest(case=label):
                self.assertTrue(continuation.looks_truncated(text))

    def test_complete_answers_are_not_continued(self):
        for text in (
            "All done.",
            "Line one\nLine two",
            "```py\nprint(1)\n```",
            "- item one\n- item two",
            "Q: why?\nA: because.",
        ):
            with self.subTest(text=text):
                self.assertFalse(continuation.looks_truncated(text))

    def test_finish_reason_length_forces_continuation(self):
        for reason in (
            "length",
            "max_tokens",
            "max_output_tokens",
            "token_limit",
            "model_context_window_exceeded",
        ):
            with self.subTest(reason=reason):
                self.assertTrue(continuation.looks_truncated("ok", reason))
        self.assertFalse(continuation.looks_truncated("ok", "stop"))

    def test_tool_call_turns_are_never_continued(self):
        self.assertFalse(
            continuation.looks_truncated("partial", "length", has_tool_calls=True)
        )

    def test_reasoning_only_turn_is_detected(self):
        self.assertTrue(
            continuation.looks_reasoning_only("", reasoning="thinking hard")
        )
        self.assertFalse(
            continuation.looks_reasoning_only("answer", reasoning="thinking")
        )
        self.assertFalse(
            continuation.looks_reasoning_only(
                "", reasoning="thinking", has_tool_calls=True
            )
        )

    def test_long_reasoning_with_fragment_is_truncated(self):
        reasoning = "step by step " * 40
        self.assertTrue(continuation.looks_truncated("The", reasoning=reasoning))
        self.assertFalse(
            continuation.looks_truncated(
                "The final answer is complete.", reasoning=reasoning
            )
        )

    def test_continuation_replays_partial_answer(self):
        messages = [{"role": "user", "content": "write a long report"}]
        continued = continuation.continued_messages(messages, "Chapter 1: ")
        self.assertEqual(continued[0], messages[0])
        self.assertEqual(
            continued[1], {"role": "assistant", "content": "Chapter 1: "}
        )
        self.assertEqual(continued[2]["role"], "user")
        self.assertEqual(
            continued[2]["content"], continuation.CONTINUE_INSTRUCTION
        )
        # The instruction must not invite a restart: the partial answer is
        # already in the history and would otherwise be duplicated.
        self.assertNotIn("\u91cd\u65b0\u5f00\u59cb\u3002\n", continued[2]["content"])

    def test_reasoning_only_uses_a_different_instruction(self):
        continued = continuation.continued_messages(
            [{"role": "user", "content": "hi"}], "", reasoning_only=True
        )
        self.assertEqual(len(continued), 2)
        self.assertIn("thinking", continued[-1]["content"].lower())

    def test_similarity_guard(self):
        self.assertEqual(continuation.similarity("abc", "abc"), 1.0)
        self.assertLess(continuation.similarity("abcdef", "abcxyz"), 0.6)

    def test_limits_come_from_the_environment(self):
        with mock.patch.dict(
            os.environ,
            {
                "TRAE_AUTO_CONTINUE": "0",
                "TRAE_AUTO_CONTINUE_MAX": "7",
                "TRAE_TRUNCATION_TEXT_THRESHOLD": "50",
                "TRAE_TRUNCATION_SIMILARITY_THRESHOLD": "0.75",
            },
        ):
            self.assertFalse(continuation.enabled())
            self.assertEqual(continuation.max_continues(), 7)
            self.assertEqual(continuation.text_threshold(), 50)
            self.assertAlmostEqual(continuation.similarity_threshold(), 0.75)

class ImageInputTests(unittest.TestCase):
    def test_openai_data_url_becomes_a_renderer_block(self):
        block = images.to_renderer_block(
            {
                "type": "image_url",
                "image_url": {"url": "data:image/png;base64,QUJD"},
            }
        )
        self.assertEqual(
            block, {"type": "image_url", "value": {"mimeType": "image/png", "data": "QUJD"}}
        )

    def test_anthropic_base64_source_becomes_a_renderer_block(self):
        block = images.to_renderer_block(
            {
                "type": "image",
                "source": {"type": "base64", "media_type": "image/jpeg", "data": "QUJD"},
            }
        )
        self.assertEqual(block["value"]["mimeType"], "image/jpeg")
        self.assertEqual(block["value"]["data"], "QUJD")

    def test_remote_url_is_preserved_rather_than_dropped(self):
        block = images.to_renderer_block(
            {"type": "image_url", "image_url": {"url": "https://example.test/a.png"}}
        )
        self.assertEqual(block["value"]["mimeType"], "url")
        self.assertEqual(block["value"]["data"], "https://example.test/a.png")

    def test_image_detection(self):
        self.assertTrue(
            images.messages_have_images(
                [
                    {
                        "role": "user",
                        "content": [
                            {"type": "text", "text": "look"},
                            {"type": "image_url", "image_url": {"url": "data:image/png;base64,QUJD"}},
                        ],
                    }
                ]
            )
        )
        self.assertFalse(
            images.messages_have_images([{"role": "user", "content": "no image"}])
        )

    def test_raw_transport_keeps_the_image_block(self):
        messages = [
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": "what is this?"},
                    {
                        "type": "image_url",
                        "image_url": {"url": "data:image/png;base64,QUJD"},
                    },
                ],
            }
        ]
        body = raw_client.build_raw_chat_body(messages, "glm-5.3", {})
        content = body["messages"][-1]["content"]
        self.assertEqual(content[0], {"type": "text", "text": "what is this?"})
        self.assertEqual(
            content[1], {"type": "image_url", "value": {"mimeType": "image/png", "data": "QUJD"}}
        )
        # The native media block carries the payload, so the text half must
        # not also describe the same image.
        self.assertNotIn("image:", content[0]["text"])

    def test_raw_transport_keeps_an_image_only_turn(self):
        messages = [
            {
                "role": "user",
                "content": [
                    {"type": "image_url", "image_url": {"url": "data:image/png;base64,QUJD"}}
                ],
            }
        ]
        body = raw_client.build_raw_chat_body(messages, "glm-5.3", {})
        content = body["messages"][-1]["content"]
        self.assertEqual(content[0]["type"], "image_url")

    def test_web_content_carries_media_parts(self):
        items = trae_client.build_web_content(
            [
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": "describe"},
                        {
                            "type": "image_url",
                            "image_url": {"url": "data:image/png;base64,QUJD"},
                        },
                    ],
                }
            ]
        )
        media = [item for item in items if item.get("type") == "image_url"]
        self.assertEqual(len(media), 1)
        self.assertEqual(media[0]["value"]["mimeType"], "image/png")

    def test_remote_query_keeps_image_media_block(self):
        query = json.loads(
            trae_client.flatten_query_parts(
                [
                    {
                        "role": "user",
                        "content": [
                            {"type": "text", "text": "describe"},
                            {
                                "type": "image_url",
                                "image_url": {
                                    "url": "data:image/png;base64,QUJD"
                                },
                            },
                        ],
                    }
                ]
            )
        )
        media = [part for part in query if part.get("type") == "image_url"]
        self.assertEqual(len(media), 1)
        self.assertEqual(media[0]["data"]["mimeType"], "image/png")
        self.assertEqual(media[0]["data"]["data"], "QUJD")

    def test_text_placeholder_never_inlines_the_payload(self):
        block = images.to_renderer_block(
            {"type": "image_url", "image_url": {"url": "data:image/png;base64,QUJD"}}
        )
        described = images.describe(block)
        self.assertNotIn("QUJD", described)

    def test_text_only_transport_describes_instead_of_dropping(self):
        text = raw_client._content_to_text(
            [
                {"type": "text", "text": "look"},
                {"type": "image_url", "image_url": {"url": "data:image/png;base64,QUJD"}},
            ]
        )
        self.assertIn("look", text)
        self.assertIn("image", text.lower())
        self.assertNotIn("Unsupported", text)


class TextToolCallCompatibilityTests(unittest.TestCase):
    """Cover the text/XML tool-call variants documented by ZedeX."""

    def test_toolcall_without_underscore_and_trailing_comma(self):
        calls = cli_client.extract_text_tool_calls(
            '<toolcall>{"name":"read_file","arguments":{"path":"a.txt",}}</toolcall>'
        )
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0]["function"]["name"], "read_file")
        self.assertEqual(
            json.loads(calls[0]["function"]["arguments"]), {"path": "a.txt"}
        )

    def test_arg_key_value_and_attribute_forms(self):
        arg_calls = cli_client.extract_text_tool_calls(
            "<tool_call>read_file"
            "<arg_key>path</arg_key><arg_value>a.txt</arg_value>"
            "</tool_call>"
        )
        self.assertEqual(arg_calls[0]["function"]["name"], "read_file")
        self.assertEqual(
            json.loads(arg_calls[0]["function"]["arguments"]), {"path": "a.txt"}
        )

        attr_calls = cli_client.extract_text_tool_calls(
            '<tool_call name="read_file" path="a.txt"></tool_call>'
        )
        self.assertEqual(attr_calls[0]["function"]["name"], "read_file")
        self.assertEqual(
            json.loads(attr_calls[0]["function"]["arguments"]), {"path": "a.txt"}
        )


class AnthropicIngressTests(unittest.TestCase):
    def test_request_translation(self):
        messages, options = anthropic_api.normalize_request(
            {
                "model": "glm-5.3",
                "max_tokens": 512,
                "system": "be terse",
                "messages": [{"role": "user", "content": "hi"}],
            }
        )
        self.assertEqual(messages[0], {"role": "system", "content": "be terse"})
        self.assertEqual(messages[1], {"role": "user", "content": "hi"})
        self.assertEqual(options["max_tokens"], 512)

    def test_tool_use_and_tool_result_keep_correlation(self):
        messages, options = anthropic_api.normalize_request(
            {
                "max_tokens": 128,
                "messages": [
                    {
                        "role": "user",
                        "content": [
                            {
                                "type": "tool_result",
                                "tool_use_id": "toolu_1",
                                "content": "file body",
                            }
                        ],
                    }
                ],
                "tools": [
                    {
                        "name": "read_file",
                        "description": "read",
                        "input_schema": {"type": "object", "properties": {}},
                    }
                ],
            }
        )
        self.assertEqual(messages[-1]["role"], "tool")
        self.assertEqual(messages[-1]["tool_call_id"], "toolu_1")
        self.assertEqual(options["tools"][0]["function"]["name"], "read_file")

    def test_tool_choice_mapping(self):
        for choice, expected in (
            ({"type": "any"}, "required"),
            ({"type": "auto"}, "auto"),
            ({"type": "none"}, "none"),
        ):
            with self.subTest(choice=choice):
                _messages, options = anthropic_api.normalize_request(
                    {
                        "max_tokens": 8,
                        "messages": [{"role": "user", "content": "x"}],
                        "tools": [{"name": "t", "input_schema": {}}],
                        "tool_choice": choice,
                    }
                )
                self.assertEqual(options["tool_choice"], expected)

    def test_thinking_budget_maps_to_a_native_level(self):
        for budget, level in ((1024, "light"), (8192, "high"), (32768, "extra_high")):
            with self.subTest(budget=budget):
                _messages, options = anthropic_api.normalize_request(
                    {
                        "max_tokens": 8,
                        "messages": [{"role": "user", "content": "x"}],
                        "thinking": {"type": "enabled", "budget_tokens": budget},
                    }
                )
                self.assertEqual(options["thinking"]["effort"], level)

    def test_image_block_survives_translation(self):
        messages, _options = anthropic_api.normalize_request(
            {
                "max_tokens": 8,
                "messages": [
                    {
                        "role": "user",
                        "content": [
                            {"type": "text", "text": "what is this"},
                            {
                                "type": "image",
                                "source": {
                                    "type": "base64",
                                    "media_type": "image/png",
                                    "data": "QUJD",
                                },
                            },
                        ],
                    }
                ],
            }
        )
        parts = messages[-1]["content"]
        self.assertEqual(parts[0], {"type": "text", "text": "what is this"})
        self.assertEqual(
            parts[1], {"type": "image_url", "image_url": {"url": "data:image/png;base64,QUJD"}}
        )

    def test_missing_messages_is_rejected(self):
        with self.assertRaises(anthropic_api.AnthropicRequestError):
            anthropic_api.normalize_request({"max_tokens": 8, "messages": []})

    def test_completion_to_message(self):
        message = anthropic_api.completion_to_message(
            {
                "model": "glm-5.3",
                "choices": [
                    {
                        "message": {
                            "role": "assistant",
                            "content": "hello",
                            "reasoning_content": "thinking",
                        },
                        "finish_reason": "stop",
                    }
                ],
                "usage": {"prompt_tokens": 3, "completion_tokens": 5},
            },
            "glm-5.3",
        )
        self.assertEqual(message["type"], "message")
        self.assertEqual([block["type"] for block in message["content"]], ["thinking", "text"])
        self.assertEqual(message["stop_reason"], "end_turn")
        self.assertEqual(message["usage"], {"input_tokens": 3, "output_tokens": 5})

    def test_completion_to_message_emits_tool_use(self):
        message = anthropic_api.completion_to_message(
            {
                "choices": [
                    {
                        "message": {
                            "role": "assistant",
                            "content": None,
                            "tool_calls": [
                                {
                                    "id": "call_1",
                                    "type": "function",
                                    "function": {
                                        "name": "read_file",
                                        "arguments": '{"path":"a.txt"}',
                                    },
                                }
                            ],
                        },
                        "finish_reason": "tool_calls",
                    }
                ]
            },
            "glm-5.3",
        )
        block = message["content"][-1]
        self.assertEqual(block["type"], "tool_use")
        self.assertEqual(block["name"], "read_file")
        self.assertEqual(block["input"], {"path": "a.txt"})
        self.assertEqual(message["stop_reason"], "tool_use")

    def test_stream_translation_emits_content_block_events(self):
        async def chat_stream():
            yield _chunk({"delta": {"role": "assistant"}})
            yield _chunk({"delta": {"content": "Hel"}})
            yield _chunk({"delta": {"content": "lo"}})
            yield _chunk(
                {"delta": {}, "finish_reason": "stop"},
                usage={"prompt_tokens": 2, "completion_tokens": 3},
            )
            yield "data: [DONE]\n\n"

        events = asyncio.run(
            _collect(anthropic_api.translate_chat_stream(chat_stream(), "glm-5.3"))
        )
        names = [_event_name(event) for event in events]
        self.assertEqual(names[0], "message_start")
        self.assertIn("content_block_start", names)
        self.assertIn("content_block_delta", names)
        self.assertIn("content_block_stop", names)
        self.assertEqual(names[-2], "message_delta")
        self.assertEqual(names[-1], "message_stop")
        text = "".join(
            _event_payload(event)["delta"]["text"]
            for event in events
            if _event_name(event) == "content_block_delta"
            and _event_payload(event)["delta"]["type"] == "text_delta"
        )
        self.assertEqual(text, "Hello")

    def test_stream_translation_emits_tool_use_json_delta(self):
        async def chat_stream():
            yield _chunk(
                {
                    "delta": {
                        "tool_calls": [
                            {
                                "index": 0,
                                "id": "call_1",
                                "function": {
                                    "name": "read_file",
                                    "arguments": '{"path":',
                                },
                            }
                        ]
                    }
                }
            )
            yield _chunk(
                {
                    "delta": {
                        "tool_calls": [
                            {"index": 0, "function": {"arguments": '"a.txt"}'}}
                        ]
                    }
                }
            )
            yield _chunk({"delta": {}, "finish_reason": "tool_calls"})
            yield "data: [DONE]\n\n"

        events = asyncio.run(
            _collect(anthropic_api.translate_chat_stream(chat_stream(), "glm-5.3"))
        )
        starts = [
            _event_payload(event)["content_block"]
            for event in events
            if _event_name(event) == "content_block_start"
        ]
        self.assertEqual(starts[-1]["type"], "tool_use")
        self.assertEqual(starts[-1]["name"], "read_file")
        partial = "".join(
            _event_payload(event)["delta"]["partial_json"]
            for event in events
            if _event_name(event) == "content_block_delta"
            and _event_payload(event)["delta"]["type"] == "input_json_delta"
        )
        self.assertEqual(json.loads(partial), {"path": "a.txt"})
        delta = _event_payload(events[-2])
        self.assertEqual(delta["delta"]["stop_reason"], "tool_use")

    def test_stream_keepalive_becomes_ping_after_message_start(self):
        async def chat_stream():
            yield ": relay-keepalive\n\n"
            yield _chunk({"delta": {"content": "x"}})
            yield "data: [DONE]\n\n"

        events = asyncio.run(
            _collect(anthropic_api.translate_chat_stream(chat_stream(), "m"))
        )
        names = [_event_name(event) for event in events]
        # Claude Code aborts a stream whose first event is not message_start,
        # so an early keepalive must open the envelope before its ping.
        self.assertEqual(names[0], "message_start")
        self.assertEqual(names[1], "ping")
        self.assertIn("content_block_delta", names)

    def test_stream_error_is_reported(self):
        async def chat_stream():
            # The relay's Chat translator emits errors as an SSE error object
            # on a data frame rather than a dedicated event name.
            yield "data: " + json.dumps(
                {"error": {"message": "upstream exploded", "type": "api_error"}}
            ) + "\n\n"
            yield "data: [DONE]\n\n"

        events = asyncio.run(
            _collect(anthropic_api.translate_chat_stream(chat_stream(), "m"))
        )
        errors = [
            _event_payload(event)["error"]["message"]
            for event in events
            if _event_name(event) == "error"
        ]
        self.assertEqual(errors, ["upstream exploded"])

    def test_stream_translation_flushes_unterminated_final_sse_block(self):
        async def chat_stream():
            payload = {
                "id": "chatcmpl-test",
                "object": "chat.completion.chunk",
                "model": "m",
                "choices": [
                    {
                        "index": 0,
                        "delta": {"content": "tail"},
                        "finish_reason": "stop",
                    }
                ],
            }
            # Deliberately omit the SSE blank-line delimiter and [DONE].
            yield "data: " + json.dumps(payload)

        events = asyncio.run(
            _collect(anthropic_api.translate_chat_stream(chat_stream(), "m"))
        )
        joined = "".join(events)
        self.assertIn('"text_delta"', joined)
        self.assertIn('"text":"tail"', joined)
        self.assertIn("event: message_stop", joined)

def _chunk(payload: dict, *, usage: dict | None = None) -> str:
    body = {
        "id": "chatcmpl-test",
        "object": "chat.completion.chunk",
        "created": 0,
        "model": "glm-5.3",
        "choices": [{"index": 0, "delta": {}, "finish_reason": None, **payload}],
    }
    if usage is not None:
        body["usage"] = usage
    return "data: " + json.dumps(body) + "\n\n"

def _event_name(event: str) -> str:
    for line in event.split("\n"):
        if line.startswith("event:"):
            return line[6:].strip()
    return ""

def _event_payload(event: str) -> dict:
    for line in event.split("\n"):
        if line.startswith("data:"):
            return json.loads(line[5:].strip())
    return {}

class _LineResponse:
    def __init__(self, lines):
        self._lines = lines

    def iter_lines(self):
        return iter(self._lines)

    def __iter__(self):
        return iter(self._lines)

async def _collect(stream):
    return [chunk async for chunk in stream]

def _parse_chunks(chunks):
    parsed = []
    done = False
    for chunk in chunks:
        if chunk == "data: [DONE]\n\n":
            done = True
            continue
        if chunk.startswith("data: "):
            parsed.append(json.loads(chunk[6:].strip()))
    return parsed, done

if __name__ == "__main__":
    unittest.main()
