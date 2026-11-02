import unittest
from unittest.mock import patch

from src.model_limits import (
    clamp_max_completion_tokens,
    descriptor_supports_max_mode,
)


class ModelDescriptorLimitTests(unittest.TestCase):
    def test_completion_limit_uses_explicit_descriptor_max_tokens(self):
        with patch.dict(
            "os.environ", {"TRAE_MAX_COMPLETION_TOKENS": "48000"}, clear=False
        ):
            self.assertEqual(
                clamp_max_completion_tokens(100000, "glm-5.3", {"max_tokens": 32000}),
                32000,
            )

    def test_completion_limit_does_not_infer_from_model_name(self):
        with patch.dict(
            "os.environ", {"TRAE_MAX_COMPLETION_TOKENS": "48000"}, clear=False
        ):
            self.assertEqual(clamp_max_completion_tokens(100000, "glm-5.3"), 48000)
            self.assertEqual(
                clamp_max_completion_tokens(100000, "glm-5.3", {"max_tokens": "unknown"}),
                48000,
            )

    def test_max_mode_requires_explicit_one_million_context_profile(self):
        common = {
            "max_mode": True,
            "prompt_max_tokens": 168000,
            "max_tokens": 32000,
        }
        self.assertFalse(
            descriptor_supports_max_mode(
                {**common, "context_window_size": {"max": 200000}}
            )
        )
        self.assertTrue(
            descriptor_supports_max_mode(
                {
                    **common,
                    "prompt_max_tokens": 936000,
                    "max_tokens": 64000,
                    "context_window_size": {"max": [200000]},
                    "context_window_tokens": {"max": [200000, 1000000]},
                }
            )
        )

    def test_max_mode_requires_all_explicit_wire_limits(self):
        descriptor = {
            "max_mode": True,
            "context_window_tokens": {"max": 1000000},
        }
        self.assertFalse(descriptor_supports_max_mode(descriptor))
        self.assertFalse(
            descriptor_supports_max_mode(
                {**descriptor, "prompt_max_tokens": 936000}
            )
        )


if __name__ == "__main__":
    unittest.main()
