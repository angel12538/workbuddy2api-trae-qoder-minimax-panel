"""Shared safety limits for model completion requests."""

from __future__ import annotations

import json
import os
from collections.abc import Mapping
from typing import Any, Optional


# Trae SOLO CN agent-remote models cap single responses at 64K tokens
# (report: solo_agent_remote max_tokens=64000). Keep the local clamp below
# that ceiling so a client asking for 131072 cannot push an upstream 4xx.
DEFAULT_MAX_COMPLETION_TOKENS = 64000

# The fallback order is only a preference among descriptors already returned
# by Trae.  It is deliberately not a capability allow-list: a model name does
# not prove that the selected account/function can read images.
MULTIMODAL_FALLBACK_ORDER = (
    "kimi-k2.6",
    "Doubao-Seed-2.1-Pro",
    "qwen3.8-max",
    "glm-5v-turbo",
    "minimax-m3",
    "kimi-k3",
    "Doubao-Seed-Code",
)


def _explicit_bool(value: Any) -> Optional[bool]:
    """Parse a descriptor flag while preserving an absent/unknown value."""

    if value is None:
        return None
    if isinstance(value, bool):
        return value
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        return bool(value)
    text = str(value).strip().lower()
    if text in {"1", "true", "yes", "on", "enabled"}:
        return True
    if text in {"0", "false", "no", "off", "disabled"}:
        return False
    return None


def _mapping(value: Any) -> Optional[Mapping[str, Any]]:
    if isinstance(value, Mapping):
        return value
    if isinstance(value, str) and value.lstrip().startswith("{"):
        try:
            parsed = json.loads(value)
        except (TypeError, ValueError):
            return None
        return parsed if isinstance(parsed, Mapping) else None
    return None


def _positive_int(value: Any) -> Optional[int]:
    if isinstance(value, str) and value.lstrip().startswith(("[", "{")):
        try:
            return _positive_int(json.loads(value))
        except (TypeError, ValueError):
            return None
    if isinstance(value, (list, tuple)):
        parsed_values = [
            parsed
            for item in value
            if (parsed := _positive_int(item)) is not None
        ]
        return max(parsed_values) if parsed_values else None
    if isinstance(value, bool):
        return None
    try:
        parsed = int(value)
    except (TypeError, ValueError):
        return None
    return parsed if parsed > 0 else None


def descriptor_multimodal(descriptor: Any) -> Optional[bool]:
    """Return the live Trae image capability, or ``None`` when unspecified.

    Catalog revisions have used top-level ``multimodal`` and
    ``support_read_image`` as well as ``features.multimodal.enable``.  An
    explicit false must win over a true value in a less authoritative nested
    field; otherwise the first field present is used.  No model-name heuristic
    is consulted here.
    """

    if not isinstance(descriptor, Mapping):
        return None
    for key in ("multimodal", "support_read_image"):
        value = _explicit_bool(descriptor.get(key))
        if value is not None:
            return value
    features = _mapping(descriptor.get("features"))
    if features is not None:
        nested = features.get("multimodal")
        if isinstance(nested, Mapping):
            value = _explicit_bool(nested.get("enable"))
        else:
            value = _explicit_bool(nested)
        if value is not None:
            return value
    return None


def is_multimodal_model(
    model: Optional[str] = None, descriptor: Any = None
) -> bool:
    """Whether the *selected descriptor* supports image input.

    ``model`` is retained for API compatibility and diagnostics only.  It is
    intentionally never used as a capability signal; callers must pass the
    descriptor returned for the bound account/function/tier.
    """

    return descriptor_multimodal(descriptor) is True


def descriptor_reasoning_config(descriptor: Any) -> Optional[dict[str, Any]]:
    """Return the current function/tier's reasoning descriptor, if any."""

    if not isinstance(descriptor, Mapping):
        return None
    config = _mapping(
        descriptor.get("reasoning_effort_config")
        or descriptor.get("reasoningEffortConfig")
    )
    if config is None:
        return None
    normalized = dict(config)
    support = _explicit_bool(
        normalized.get("support_thinking")
        if "support_thinking" in normalized
        else normalized.get("supportThinking")
    )
    if support is not None:
        normalized["support_thinking"] = support
    if "options" not in normalized:
        options = normalized.get("reasoning_effort_options")
        if options is None:
            options = normalized.get("reasoningEffortOptions")
        if options is not None:
            normalized["options"] = options
    options = normalized.get("options")
    if isinstance(options, str) and options.lstrip().startswith("["):
        try:
            parsed_options = json.loads(options)
        except (TypeError, ValueError):
            parsed_options = None
        if isinstance(parsed_options, list):
            normalized["options"] = parsed_options
    return normalized


def descriptor_context_limits(descriptor: Any) -> dict[str, Any]:
    """Extract only limits explicitly present in a live model descriptor.

    This function never manufactures 1M/936K/64K values.  It is used by both
    Remote and native transports so an absent max profile stays absent.
    """

    if not isinstance(descriptor, Mapping):
        return {}
    result: dict[str, Any] = {}
    for key in (
        "max_mode",
        "context_window_size",
        "context_window_sizes",
        "context_window_tokens",
        "prompt_max_tokens",
        "max_tokens",
        "max_turn",
        "max_turns",
    ):
        if key in descriptor and descriptor.get(key) is not None:
            result[key] = descriptor.get(key)
    features = _mapping(descriptor.get("features"))
    feature_windows = (
        _mapping(features.get("context_windows"))
        if features is not None
        else None
    )
    if feature_windows is not None:
        result["context_windows"] = dict(feature_windows)
    return result


def descriptor_max_context(descriptor: Any) -> Optional[int]:
    """Return the explicitly advertised max context for this descriptor."""

    limits = descriptor_context_limits(descriptor)
    candidates: list[int] = []
    for key in ("context_window_size", "context_window_sizes"):
        value = limits.get(key)
        value_map = _mapping(value)
        if value_map is not None:
            value = value_map.get("max") or value_map.get("max_context")
        parsed = _positive_int(value)
        if parsed is not None:
            candidates.append(parsed)
    tokens = _mapping(limits.get("context_window_tokens"))
    if tokens is not None:
        parsed = _positive_int(tokens.get("max"))
        if parsed is not None:
            candidates.append(parsed)
    windows = _mapping(limits.get("context_windows"))
    if windows is not None:
        data = _mapping(windows.get("data"))
        if data is not None:
            for key in ("max_context", "max_context_list"):
                parsed = _positive_int(data.get(key))
                if parsed is not None:
                    candidates.append(parsed)
    return max(candidates) if candidates else None


def descriptor_supports_max_mode(descriptor: Any) -> bool:
    """Return true only for an explicitly advertised 1M Max profile."""

    if not isinstance(descriptor, Mapping):
        return False
    enabled = _explicit_bool(descriptor.get("max_mode"))
    if enabled is not True:
        return False
    limits = descriptor_context_limits(descriptor)
    # Max is a concrete profile, not a model-name feature.  The current
    # function must explicitly provide all three wire limits before the relay
    # can select it; otherwise server defaults would recreate the prohibited
    # synthetic 1M/936K/64K profile.
    max_context = descriptor_max_context(descriptor)
    return bool(
        max_context is not None
        and max_context >= 1_000_000
        and _positive_int(limits.get("prompt_max_tokens"))
        and _positive_int(limits.get("max_tokens"))
    )


def configured_multimodal_model() -> str:
    return str(os.environ.get("TRAE_MULTIMODAL_MODEL", "")).strip()


def _configured_limit(model: Optional[str] = None, descriptor: Any = None) -> int:
    raw = os.environ.get("TRAE_MAX_COMPLETION_TOKENS", "")
    try:
        value = int(raw)
    except (TypeError, ValueError):
        value = DEFAULT_MAX_COMPLETION_TOKENS
    if value <= 0:
        value = DEFAULT_MAX_COMPLETION_TOKENS
    if isinstance(descriptor, Mapping):
        raw_max = descriptor.get("max_tokens")
        try:
            max_tokens = int(raw_max)
        except (TypeError, ValueError):
            max_tokens = 0
        if max_tokens > 0:
            return min(value, max_tokens)
    return value


def clamp_max_completion_tokens(
    value: Any, model: Optional[str] = None, descriptor: Any = None
) -> Any:
    """Clamp positive completion-token values to the upstream-safe limit."""

    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return value
    if value <= 0:
        return value
    return min(int(value), _configured_limit(model, descriptor))
