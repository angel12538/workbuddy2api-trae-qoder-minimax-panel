#!/usr/bin/env python3
"""Build the generic authorization-helper assets shipped on a GitHub Release.

These optional fallback copies default to a local deployment. The console
serves deployment-specific copies directly; normal users need no GitHub
download or manual address changes.

Usage::

    python scripts/build_auth_helper_assets.py --version 1.0.5 --output dist
"""

from __future__ import annotations

import argparse
import hashlib
import zipfile
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from src.auth_helper import DEFAULT_LISTENER_PORT, DEFAULT_RELAY_URL, build_helper_text  # noqa: E402

ASSET_KINDS = ("py", "bat", "sh")


def _asset_text(kind: str, *, version: str, listener_port: int) -> str:
    template = ""
    if kind == "py":
        template = (REPO_ROOT / "web_login.py").read_text(encoding="utf-8")
    return build_helper_text(
        kind,
        template,
        # The normal console download supplies its actual deployment URL.
        base_url="",
        listener_port=listener_port,
        generated_at="通用版",
        version=version,
    )


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True, help="Release version, with or without a leading v")
    parser.add_argument("--output", default="dist", help="Directory for generated assets")
    parser.add_argument(
        "--listener-port",
        type=int,
        default=DEFAULT_LISTENER_PORT,
        help="Local callback listener port baked into the scripts",
    )
    parser.add_argument(
        "--sync-repo",
        action="store_true",
        help="Also refresh the helper copies at the repository root (shipped in the image)",
    )
    args = parser.parse_args()

    version = args.version.strip().lstrip("v")
    output_dir = Path(args.output)
    output_dir.mkdir(parents=True, exist_ok=True)

    written: list[Path] = []
    for kind in ASSET_KINDS:
        text = _asset_text(kind, version=version, listener_port=args.listener_port)
        # 稳定文件名：releases/latest/download/<name> 直接可用。
        name = {"py": "web_login.py", "bat": "start_auth.bat", "sh": "start_auth.sh"}[kind]
        target = output_dir / name
        newline = "\r\n" if kind == "bat" else "\n"
        body = text.replace("\r\n", "\n").replace("\n", newline)
        target.write_bytes(body.encode("utf-8"))
        written.append(target)
        print(f"wrote {target} ({target.stat().st_size} bytes)")
        if args.sync_repo:
            repo_copy = REPO_ROOT / name
            repo_copy.write_bytes(body.encode("utf-8"))
            print(f"synced {repo_copy}")

    # 一次性打包，方便只下载一个附件的用户。
    bundle = output_dir / f"trae-auth-helper-{version}.zip"
    with zipfile.ZipFile(bundle, "w", zipfile.ZIP_DEFLATED) as archive:
        for path in written:
            archive.write(path, path.name)
        archive.writestr(
            "README-auth-helper.txt",
            (
                "Trae2api-cn 授权助手（通用版）\n\n"
                "推荐用法：在浏览器打开中转站控制台，直接点「授权助手」按钮下载，"
                "服务端会把当前部署地址和端口写进文件，下载后无需修改。\n"
                "本压缩包仅作本机默认部署的备用；其他地址推荐直接从部署控制台下载。\n"
                "仅源码通用版需显式指定：python web_login.py --relay http://<host>:<port>\n"
            ),
        )
    print(f"wrote {bundle} ({bundle.stat().st_size} bytes)")
    written.append(bundle)

    checksum_file = output_dir / f"trae-cn-relay-{version}.sha256"
    if checksum_file.exists():
        lines = [line for line in checksum_file.read_text(encoding="utf-8").splitlines() if line.strip()]
        lines = [line for line in lines if not any(line.endswith(f"  {path.name}") for path in written)]
        lines.extend(f"{_sha256(path)}  {path.name}" for path in written)
        checksum_file.write_text("\n".join(lines) + "\n", encoding="utf-8")
        print(f"updated {checksum_file}")

    # 助手资产单独的校验文件，方便只核对这几个附件。
    sidecar = output_dir / f"trae-auth-helper-{version}.sha256"
    sidecar.write_text(
        "\n".join(f"{_sha256(path)}  {path.name}" for path in written) + "\n",
        encoding="utf-8",
    )
    print(f"wrote {sidecar}")

    print(f"generic helper default relay: {DEFAULT_RELAY_URL}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
