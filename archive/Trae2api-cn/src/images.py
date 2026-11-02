"""Normalize caller image inputs into Trae's renderer media block shape.

The desktop renderer carries media as ``{"type":"image_url",
"value":{"mimeType":...,"data":...}}`` (reverse-engineered NPi converter).
OpenAI clients send ``image_url`` parts with a data URL or http URL, and
Anthropic clients send base64 ``source`` objects.  This module converts both
into the single block shape the upstream transports understand, and reports
whether a request carries images so the dispatcher can route it to a
multimodal model.
"""

from __future__ import annotations

import base64
import re
from typing import Any, Mapping, Optional

_DATA_URL_RE = re.compile(
    r"^data:(?P<mime>[-\w.+/]+);base64,(?P<data>[A-Za-z0-9+/=\s]+)$",
    re.DOTALL,
)
_DEFAULT_MIME = "image/png"


def _split_data_url(url: str) -> tuple[str, str]:
    match = _DATA_URL_RE.match((url or "").strip())
    if not match:
        return "", ""
    return match.group("mime"), re.sub(r"\s+", "", match.group("data"))


def renderer_block(
    data: str, mime_type: str = _DEFAULT_MIME
) -> dict[str, Any]:
    """Return one Trae renderer media block."""

    return {
        "type": "image_url",
        "value": {
            "mimeType": str(mime_type or _DEFAULT_MIME),
            "data": str(data or ""),
        },
    }


def from_openai_part(part: Mapping[str, Any]) -> Optional[dict[str, Any]]:
    """Convert an OpenAI ``image_url`` content part, or return ``None``."""

    image = part.get("image_url")
    url = ""
    if isinstance(image, Mapping):
        url = str(image.get("url") or "")
    elif isinstance(image, str):
        url = image
    if not url:
        url = str(part.get("url") or "")
    if not url:
        return None
    if url.startswith("data:"):
        mime, data = _split_data_url(url)
        if not data:
            return None
        return renderer_block(data, mime or _DEFAULT_MIME)
    # A remote URL cannot be inlined by the relay, but forwarding it verbatim
    # is still better than silently dropping the caller's image.
    return renderer_block(url, "url")


def from_anthropic_block(block: Mapping[str, Any]) -> Optional[dict[str, Any]]:
    """Convert an Anthropic ``image`` block, or return ``None``."""

    source = block.get("source")
    if not isinstance(source, Mapping):
        return None
    source_type = str(source.get("type") or "").strip().lower()
    if source_type == "base64":
        data = re.sub(r"\s+", "", str(source.get("data") or ""))
        if not data:
            return None
        return renderer_block(data, str(source.get("media_type") or _DEFAULT_MIME))
    if source_type == "url":
        url = str(source.get("url") or "")
        if url:
            return renderer_block(url, "url")
    return None


def is_image_part(part: Any) -> bool:
    if not isinstance(part, Mapping):
        return False
    part_type = str(part.get("type") or "").strip().lower()
    return part_type in ("image_url", "input_image", "image")


def message_has_image(message: Any) -> bool:
    """Whether one chat message carries any image part."""

    if not isinstance(message, Mapping):
        return False
    content = message.get("content")
    if not isinstance(content, list):
        return False
    for part in content:
        if not isinstance(part, Mapping):
            continue
        if str(part.get("type") or "").strip().lower() == "image":
            if isinstance(part.get("source"), Mapping):
                return True
            continue
        if is_image_part(part):
            if part.get("image_url") or part.get("url") or part.get("image"):
                return True
    return False


def messages_have_images(messages: Any) -> bool:
    if not isinstance(messages, (list, tuple)):
        return False
    return any(message_has_image(message) for message in messages)


def to_renderer_block(part: Mapping[str, Any]) -> Optional[dict[str, Any]]:
    """Convert either OpenAI or Anthropic image part into a renderer block."""

    part_type = str(part.get("type") or "").strip().lower()
    if part_type == "image":
        return from_anthropic_block(part)
    return from_openai_part(part)




def describe(block: Mapping[str, Any]) -> str:
    """A short, non-secret label used when a transport cannot carry media."""

    value = block.get("value")
    value = value if isinstance(value, Mapping) else {}
    mime = str(value.get("mimeType") or _DEFAULT_MIME)
    data = str(value.get("data") or "")
    if mime == "url" or data.startswith("http"):
        return f"[image: {data[:120]}]"
    try:
        size = len(base64.b64decode(data + "=" * (-len(data) % 4)))
    except Exception:
        size = 0
    if size:
        return f"[image: {mime}, {size} bytes]"
    return f"[image: {mime}]"


def query_parts(messages: Any) -> list[dict[str, Any]]:
    """Return Trae's ``query`` part array for a message list.

    The remote transport sends history as a JSON array of ``{"type","data"}``
    parts.  Text is ``{"type":"text","data":{"content":...}}``; an image is the
    renderer media payload wrapped the same way, which keeps the model's
    multimodal input available instead of flattening the attachment to a
    caption.  A short text placeholder accompanies every image so a
    non-multimodal reader still knows something was attached.
    """

    if not isinstance(messages, (list, tuple)):
        return []
    parts: list[dict[str, Any]] = []
    for message in messages:
        if not isinstance(message, Mapping):
            continue
        content = message.get("content")
        if not isinstance(content, list):
            continue
        for part in content:
            if not isinstance(part, Mapping):
                continue
            if not is_image_part(part) and str(part.get("type") or "") != "image":
                continue
            block = to_renderer_block(part)
            if not block:
                continue
            value = block.get("value")
            value = value if isinstance(value, Mapping) else {}
            parts.append(
                {
                    "type": "image_url",
                    "data": {
                        "mimeType": value.get("mimeType") or _DEFAULT_MIME,
                        "data": value.get("data") or "",
                    },
                }
            )
            parts.append(
                {
                    "type": "text",
                    "data": {"content": describe(block)},
                }
            )
    return parts
