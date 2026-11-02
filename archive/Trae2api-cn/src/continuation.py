"""Optional truncation helpers for explicit caller-managed diagnostics.

Production Chat/Anthropic handlers do not implicitly re-issue a model turn:
the caller owns continuation so a truncated response cannot silently duplicate
billable work or replay a tool turn.  The detection and message-building
helpers remain available for tests and explicitly opted-in internal callers.
"""

from __future__ import annotations

import os
import re
from typing import Any, Mapping, Optional, Sequence

# Every alias the relay's translators and Trae itself use for "the model ran
# out of output budget".  Missing one would hand the caller a half answer.
_LENGTH_FINISH_REASONS = frozenset(
    {
        "length",
        "max_tokens",
        "max_output_tokens",
        "max_token",
        "token_limit",
        "model_context_window_exceeded",
        "context_window_exceeded",
        "context_length_exceeded",
    }
)

_DEFAULT_TEXT_THRESHOLD = 200
_DEFAULT_SIMILARITY_THRESHOLD = 0.5
_DEFAULT_MAX_CONTINUES = 3

CONTINUE_INSTRUCTION = (
    "请继续输出，从你中断的地方继续，不要重复已经写过的内容，不要重新开始。"
)
REASONING_ONLY_INSTRUCTION = (
    "Your previous response contained only thinking/reasoning with no actual "
    "output. Provide the actual response now - either text content or a tool call."
)

# Trailing characters that prove the model stopped mid-token/structure.
_TRUNCATED_ENDINGS = (
    re.compile(r",\s*$"),
    re.compile(r"[|｜]\s*$"),
    re.compile(r"\.\.\.\s*$"),
    re.compile(r"\\\s*$"),
    re.compile(r"//\s*$"),
    re.compile(r"#\s*$"),
    re.compile(r"[-–—]\s*$"),
    re.compile(r"[:：]\s*$"),
)


def _env_flag(name: str, default: bool) -> bool:
    raw = os.environ.get(name)
    if raw in (None, ""):
        return default
    return str(raw).strip().lower() not in {"0", "false", "no", "off", "disable", "disabled"}


def _env_int(name: str, default: int, minimum: int = 0) -> int:
    try:
        value = int(str(os.environ.get(name, "")).strip())
    except (TypeError, ValueError):
        return default
    return value if value >= minimum else default


def _env_float(name: str, default: float) -> float:
    try:
        value = float(str(os.environ.get(name, "")).strip())
    except (TypeError, ValueError):
        return default
    return value


def enabled() -> bool:
    """Whether the relay may transparently continue a truncated answer."""

    # Automatic re-issue was introduced by the Zedex compatibility layer.
    # It can duplicate billable turns and causes providers that require a
    # single tool turn to reject the follow-up.  Keep the helper available for
    # explicit diagnostics, but production defaults to the provider's terminal
    # response; operators must opt in with TRAE_AUTO_CONTINUE=1.
    return _env_flag("TRAE_AUTO_CONTINUE", False)


def max_continues() -> int:
    return _env_int("TRAE_AUTO_CONTINUE_MAX", _DEFAULT_MAX_CONTINUES, 0)


def text_threshold() -> int:
    return _env_int(
        "TRAE_TRUNCATION_TEXT_THRESHOLD", _DEFAULT_TEXT_THRESHOLD, 1
    )


def similarity_threshold() -> float:
    value = _env_float(
        "TRAE_TRUNCATION_SIMILARITY_THRESHOLD", _DEFAULT_SIMILARITY_THRESHOLD
    )
    return min(max(value, 0.0), 1.0)


def similarity(left: str, right: str) -> float:
    """Character-position similarity used to stop a repeated short answer."""

    left = left or ""
    right = right or ""
    overlap = min(len(left), len(right))
    if overlap <= 0:
        return 0.0
    same = sum(1 for index in range(overlap) if left[index] == right[index])
    return same / overlap


def _unclosed_code_fence(text: str) -> bool:
    return text.count("```") % 2 == 1


def _unbalanced_brackets(tail: str) -> bool:
    """Whether a code-shaped tail is missing closing brackets.

    A plain count difference needs a small tolerance (prose and tables contain
    stray brackets), but a tail that *ends* inside an open bracket group is
    unambiguous: nothing after it could close the group.
    """

    opens = sum(tail.count(char) for char in "[{(")
    closes = sum(tail.count(char) for char in "]})")
    if opens > closes + 2:
        return True
    # Walk the tail and require every closer to match the innermost opener, so
    # ``{a: [1, 2}`` is not mistaken for balanced output.
    stack: list[str] = []
    pairs = {")": "(", "]": "[", "}": "{"}
    for char in tail:
        if char in "[{(":
            stack.append(char)
        elif char in pairs:
            if not stack or stack[-1] != pairs[char]:
                # Mismatched closer: the structure is already broken, which is
                # exactly the mid-write state this detector looks for.
                return True
            stack.pop()
    return bool(stack)


_SENTENCE_END_RE = re.compile(r"[.!?。！？\]\)\}`\"'\u201d\u2019]\s*$")
_FRAGMENT_TEXT_LIMIT = 40
_FRAGMENT_REASONING_MIN = 200
_FRAGMENT_REASONING_RATIO = 4


def _stopped_after_thinking(text: str, reasoning: str) -> bool:
    """A long reasoning trace followed by a sentence fragment.

    The model can burn its whole turn inside the thinking channel and then
    emit only one or two tokens of the answer.  Treating that fragment as the
    final answer hides the fact that nothing was actually written, so it is
    continued once (the caller's similarity guard stops a repeat loop).
    """

    visible = (text or "").strip()
    thinking = (reasoning or "").strip()
    if not visible or len(visible) > _FRAGMENT_TEXT_LIMIT:
        return False
    if len(thinking) < _FRAGMENT_REASONING_MIN:
        return False
    if len(thinking) < len(visible) * _FRAGMENT_REASONING_RATIO:
        return False
    return not _SENTENCE_END_RE.search(visible)


def looks_truncated(
    text: str,
    finish_reason: Optional[str] = None,
    *,
    has_tool_calls: bool = False,
    reasoning: str = "",
) -> bool:
    """Return whether a completed upstream turn looks cut off mid-answer.

    Tool-call turns are never continued: the caller owns execution and must
    receive the call as-is.
    """

    if has_tool_calls:
        return False
    reason = str(finish_reason or "").strip().lower()
    if reason in _LENGTH_FINISH_REASONS:
        return True
    body = text or ""
    if not body.strip():
        # Nothing public was produced.  Reasoning-only output is the one case
        # worth re-asking; a totally empty turn is handled by the existing
        # empty-response retry/fallback path instead.
        return bool((reasoning or "").strip())
    if _unclosed_code_fence(body):
        return True
    if _unbalanced_brackets(body[-100:]):
        return True
    tail = body[-100:].rstrip()
    for pattern in _TRUNCATED_ENDINGS:
        if pattern.search(tail):
            return True
    return _stopped_after_thinking(body, reasoning)


def looks_reasoning_only(
    text: str, *, has_tool_calls: bool = False, reasoning: str = ""
) -> bool:
    if has_tool_calls:
        return False
    return not (text or "").strip() and bool((reasoning or "").strip())


def continued_messages(
    messages: Sequence[Mapping[str, Any]],
    partial_text: str,
    *,
    reasoning_only: bool = False,
) -> list[dict[str, Any]]:
    """Return the conversation for one continuation round.

    The partial answer is replayed as an assistant turn so the upstream keeps
    its own voice, followed by a user turn that only asks for the remainder.
    """

    continued: list[dict[str, Any]] = [
        dict(message) for message in messages if isinstance(message, Mapping)
    ]
    partial = (partial_text or "").strip()
    if partial:
        continued.append({"role": "assistant", "content": partial_text})
    instruction = REASONING_ONLY_INSTRUCTION if reasoning_only else CONTINUE_INSTRUCTION
    continued.append({"role": "user", "content": instruction})
    return continued
