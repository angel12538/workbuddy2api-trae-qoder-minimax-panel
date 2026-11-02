import copy
import unittest
from unittest.mock import patch

from src import main as main_module


def _sequential_suffix(messages, limit):
    bounded = copy.deepcopy(messages)
    removed = 0
    while len(main_module.trae_client.flatten_query_parts(bounded)) > limit:
        indexes = [
            index for index, message in enumerate(bounded)
            if message.get("role") not in {"system", "developer"}
        ]
        if len(indexes) <= 1:
            return None
        head = indexes[0]
        drop = {head}
        if bounded[head].get("role") == "assistant" and (
            bounded[head].get("tool_calls") or bounded[head].get("function_call")
        ):
            if head + 1 < len(bounded) and bounded[head + 1].get("role") == "tool":
                drop.add(head + 1)
        bounded = [message for index, message in enumerate(bounded) if index not in drop]
        removed += len(drop)
    return bounded, removed


def _image(size):
    return {
        "type": "image_url",
        "image_url": {"url": "data:image/png;base64," + "A" * size},
    }


class RemoteQueryCompactionTests(unittest.TestCase):
    def test_compaction_keeps_the_same_first_fitting_suffix(self):
        call = {
            "role": "assistant", "content": "calling",
            "tool_calls": [{"id": "c", "function": {"name": "read", "arguments": "{}"}}],
        }
        result = {"role": "tool", "tool_call_id": "c", "content": "done"}
        cases = [
            [{"role": "user", "content": "x" * 300}, call, result,
             {"role": "user", "content": "continue"}],
            [{"role": "system", "content": "rules"},
             {"role": "user", "content": "x" * 300},
             {"role": "developer", "content": "more rules"}, call, result,
             {"role": "user", "content": "continue"}],
            [{"role": "user", "content": "x" * 300}, call,
             {"role": "system", "content": "between call and result"}, result,
             {"role": "user", "content": "continue"}],
            [{"role": "user", "content": [_image(800)]}, call, result,
             {"role": "user", "content": [_image(8)]}],
            [{"role": "system", "content": "rules"}, call, result],
        ]
        for messages in cases:
            original = copy.deepcopy(messages)
            for limit in (100, 220, 500, 1_200):
                with self.subTest(messages=messages, limit=limit):
                    expected = _sequential_suffix(messages, limit)
                    if expected is None:
                        continue
                    actual = main_module._bounded_remote_query(
                        messages, {"trae_remote_query_max_chars": limit}
                    )
                    self.assertEqual(actual, expected)
                    self.assertEqual(messages, original)

    def test_long_image_history_requires_only_bounded_serialization_work(self):
        image = _image(100_000)
        messages = [{"role": "system", "content": "rules"}]
        messages.extend(
            {"role": "assistant" if index % 2 else "user", "content": "x" * 2_000}
            for index in range(600)
        )
        messages.append({"role": "user", "content": [{"type": "text", "text": "latest"}, image]})
        flatten = main_module.trae_client.flatten_query_parts
        with patch.object(main_module.trae_client, "flatten_query_parts", wraps=flatten) as encoded:
            bounded, removed = main_module._bounded_remote_query(
                messages, {"trae_remote_query_max_chars": 120_000}
            )
        self.assertLessEqual(encoded.call_count, 12)
        self.assertGreater(removed, 500)
        self.assertEqual(bounded[0], messages[0])
        self.assertEqual(bounded[-1], messages[-1])
        self.assertLessEqual(len(flatten(bounded)), 120_000)
        if bounded[1]["role"] != "system":
            previous = [messages[0], messages[removed], *bounded[1:]]
            self.assertGreater(len(flatten(previous)), 120_000)

    def test_image_overflow_does_not_repeat_text_fitting(self):
        messages = [{"role": "user", "content": [{"type": "text", "text": "x" * 20_000}, _image(600_000)]}]
        original = copy.deepcopy(messages)
        flatten = main_module.trae_client.flatten_query_parts
        with (
            patch.object(main_module.trae_client, "flatten_query_parts", wraps=flatten) as encoded,
            patch.object(main_module, "_fit_remote_query_contents") as fit,
        ):
            with self.assertRaises(main_module.RemoteQueryTooLarge) as error:
                main_module._bounded_remote_query(
                    messages, {"trae_remote_query_max_chars": 480_000}
                )
        fit.assert_not_called()
        self.assertEqual(encoded.call_count, 1)
        self.assertGreater(error.exception.query_chars, 600_000)
        self.assertEqual(messages, original)

    def test_renderer_tool_name_inheritance_preserves_nonmonotonic_suffixes(self):
        call = {"role": "assistant", "tool_calls": [
            {"id": "c", "function": {"name": "x", "arguments": "{}"}}
        ]}
        tail = [call, {"role": "tool", "tool_call_id": "c", "content": "ok"}]
        tail.extend(
            {"role": "user", "content": [{"type": "tool_result", "toolCallId": "c", "value": "ok"}]}
            for _ in range(100)
        )
        tail.append({"role": "user", "content": "continue"})
        flatten = main_module.trae_client.flatten_query_parts
        limit = len(flatten(tail))
        self.assertGreater(len(flatten(tail[2:])), limit)
        messages = [{"role": "user", "content": "x" * 1_000}, *tail]
        bounded, removed = main_module._bounded_remote_query(
            messages, {"trae_remote_query_max_chars": limit}
        )
        self.assertEqual(removed, 1)
        self.assertEqual(bounded, tail)


if __name__ == "__main__":
    unittest.main()
