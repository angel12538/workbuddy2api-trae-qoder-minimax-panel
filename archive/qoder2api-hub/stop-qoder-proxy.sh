#!/usr/bin/env bash
# ===========================================================
#  Qoder proxy - stop the running instance   [Linux / macOS]
#
#  issue #89: macOS/Linux used to have no official stop path
#  (start-qoder-proxy.sh never wrote a pid file). This mirrors
#  stop-qoder-proxy.bat: read the pid file, verify the command
#  line really is qoder_proxy.py, then stop THAT pid only.
#
#  Usage:  ./stop-qoder-proxy.sh [port]
# ===========================================================
set -u

if [ -z "${BASH_VERSION:-}" ]; then
    exec bash "$0" "$@"
fi

PORT="${1:-8790}"
SRC="${BASH_SOURCE[0]:-$0}"
HERE="$(cd -- "$(dirname -- "$SRC")" && pwd)"
PIDFILE="$HERE/qoder-proxy.pid"

# A pid belongs to qoder-proxy only when its command line says so.
is_qoder_proxy() {
    _pid="$1"
    _cmd="$(ps -p "$_pid" -o command= 2>/dev/null || true)"
    case "$_cmd" in
        *qoder_proxy.py*) return 0 ;;
    esac
    return 1
}

# A zombie (defunct) process still answers kill -0 but is effectively gone;
# without this check the wait loop below would spin 10s and then report failure
# just because the parent has not reaped it yet.
pid_gone() {
    _p="$1"
    if ! kill -0 "$_p" 2>/dev/null; then
        return 0
    fi
    _st="$(ps -p "$_p" -o state= 2>/dev/null | tr -d ' 	')"
    if [ -z "$_st" ]; then
        return 0
    fi
    case "$_st" in
        Z*) return 0 ;;
    esac
    return 1
}

stop_pid() {
    _pid="$1"
    kill -TERM "$_pid" 2>/dev/null || true
    _i=0
    while [ "$_i" -lt 20 ]; do
        if pid_gone "$_pid"; then
            return 0
        fi
        sleep 0.5
        _i=$((_i + 1))
    done
    echo "[WARN] PID $_pid did not exit after 10s - sending SIGKILL"
    kill -KILL "$_pid" 2>/dev/null || true
    sleep 0.5
    pid_gone "$_pid"
}

PID=""
if [ -f "$PIDFILE" ]; then
    PID="$(tr -d '[:space:]' < "$PIDFILE" 2>/dev/null || true)"
fi

if [ -n "$PID" ]; then
    if ! kill -0 "$PID" 2>/dev/null; then
        echo "[INFO] pid $PID from $PIDFILE is not running (stale pid file) - removing it."
        rm -f "$PIDFILE" 2>/dev/null || true
        PID=""
    elif ! is_qoder_proxy "$PID"; then
        echo "[WARN] PID $PID is not qoder-proxy, refusing to stop it."
        echo "       command: $(ps -p "$PID" -o command= 2>/dev/null)"
        exit 3
    fi
fi

# Fallback: the pid file may be missing (older start script / manual run).
# Only accept processes whose command line contains BOTH qoder_proxy.py and
# this port, so an editor or another checkout is never killed by accident.
if [ -z "$PID" ]; then
    for _cand in $(ps -eo pid=,command= 2>/dev/null | \
                   grep -F "qoder_proxy.py" | grep -F -- "--port $PORT" | \
                   grep -v grep | awk '{print $1}'); do
        if is_qoder_proxy "$_cand"; then
            PID="$_cand"
            echo "[INFO] pid file missing - found qoder-proxy on port $PORT (PID $PID)"
            break
        fi
    done
fi

if [ -z "$PID" ]; then
    echo "[INFO] qoder-proxy does not appear to be running on port $PORT."
    echo "       If you started it manually in a terminal, press Ctrl+C there."
    exit 2
fi

if stop_pid "$PID"; then
    rm -f "$PIDFILE" 2>/dev/null || true
    echo "[OK] stopped qoder-proxy (PID $PID)"
    exit 0
fi

echo "[ERROR] failed to stop PID $PID"
exit 4