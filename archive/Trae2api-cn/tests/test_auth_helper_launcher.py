"""Exercise generated launchers with a local, credential-free download server."""

import json
import os
import shutil
import stat
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from src import auth_helper


@pytest.mark.parametrize("kind", ["bat", "sh"])
@pytest.mark.parametrize(
    "download_succeeds,existing_helper,replace_fails,target_is_directory",
    [
        (True, False, False, False),
        (True, True, False, False),
        (False, False, False, False),
        (False, True, False, False),
        (True, True, True, False),
        (True, True, False, True),
    ],
)
def test_launcher_refreshes_next_to_itself_and_handles_partial_failure(
    tmp_path, kind, download_succeeds, existing_helper, replace_fails, target_is_directory
):
    if kind == "bat" and os.name != "nt":
        pytest.skip("Windows batch launcher")
    if kind == "sh" and (os.name == "nt" or not shutil.which("sh")):
        pytest.skip("POSIX shell launcher")
    if not shutil.which("curl"):
        pytest.skip("curl is not installed")
    requested = []
    helper_source = (
        "import json, sys\n"
        "print('LAUNCH_RESULT=' + json.dumps(sys.argv[1:]))\n"
    ).encode("utf-8")

    class DownloadHandler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_GET(self):
            requested.append(self.path)
            self.send_response(200)
            self.send_header("Content-Length", str(len(helper_source)))
            self.end_headers()
            if download_succeeds:
                self.wfile.write(helper_source)
            else:
                # curl must fail for an incomplete response instead of leaving
                # a seemingly valid helper for the launcher to execute.
                self.wfile.write(helper_source[:10])
                self.close_connection = True

    server = ThreadingHTTPServer(("127.0.0.1", 0), DownloadHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        relay_url = f"http://127.0.0.1:{server.server_address[1]}"
        download_dir = tmp_path / "download folder"
        download_dir.mkdir()
        unrelated_dir = tmp_path / "unrelated"
        unrelated_dir.mkdir()
        launcher = download_dir / f"start_auth.{kind}"
        helper = download_dir / "web_login.py"
        old_helper_source = b"print('STALE_HELPER_EXECUTED')\n"
        if target_is_directory:
            helper.mkdir()
            (helper / "__main__.py").write_bytes(old_helper_source)
        elif existing_helper:
            helper.write_bytes(old_helper_source)
        environment = os.environ.copy()
        if replace_fails:
            if kind == "bat":
                helper.chmod(stat.S_IREAD)
            else:
                bin_dir = tmp_path / "bin"
                bin_dir.mkdir()
                failed_move = bin_dir / "mv"
                failed_move.write_text("#!/bin/sh\nexit 1\n", encoding="utf-8")
                failed_move.chmod(0o755)
                environment["PATH"] = str(bin_dir) + os.pathsep + environment["PATH"]
        launcher.write_bytes(
            auth_helper.render(kind, relay_url=relay_url, listener_port=9321).encode("utf-8")
        )
        command = (
            [os.environ.get("COMSPEC", "cmd.exe"), "/d", "/c", str(launcher)]
            if kind == "bat" else [shutil.which("sh"), str(launcher)]
        )
        result = subprocess.run(
            command,
            env=environment,
            cwd=unrelated_dir,
            input="",
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=15,
        )
        assert requested == ([] if target_is_directory else ["/web/login/download?as=py"])
        assert not list(download_dir.glob("*.download*"))
        assert not (unrelated_dir / "web_login.py").exists()
        assert "STALE_HELPER_EXECUTED" not in result.stdout
        if target_is_directory:
            assert result.returncode != 0
            assert "LAUNCH_RESULT=" not in result.stdout
            assert (helper / "__main__.py").read_bytes() == old_helper_source
        elif download_succeeds and not replace_fails:
            assert result.returncode == 0, result.stdout + result.stderr
            output = next(line for line in result.stdout.splitlines() if line.startswith("LAUNCH_RESULT="))
            assert json.loads(output.split("=", 1)[1]) == [
                "--relay", relay_url, "--port", "9321"
            ]
            assert helper.read_bytes() == helper_source
        else:
            assert result.returncode != 0
            assert "LAUNCH_RESULT=" not in result.stdout
            if existing_helper:
                assert helper.read_bytes() == old_helper_source
            else:
                assert not helper.exists()
    finally:
        if replace_fails and kind == "bat" and helper.exists():
            helper.chmod(stat.S_IREAD | stat.S_IWRITE)
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
