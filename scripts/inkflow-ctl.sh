#!/bin/sh
# inkflow-ctl.sh — start/stop/restart/status control for inkflow on Synology DSM.
#
# DSM has no systemd, so this uses a PID file + nohup instead of a service unit.
# Typical layout on the NAS:
#   /volume1/homes/<user>/inkflow/inkflow-synology-ds918   (binary)
#   /volume1/homes/<user>/inkflow/inkflow.toml             (config)
#   /volume1/homes/<user>/inkflow/inkflow.pid              (pid file, created here)
#   /volume1/homes/<user>/inkflow/inkflow.log              (stdout/stderr)
#
# Usage:
#   ./inkflow-ctl.sh start      # start if not already running
#   ./inkflow-ctl.sh stop       # stop if running
#   ./inkflow-ctl.sh restart    # stop (if running) then start — use after updating the binary
#   ./inkflow-ctl.sh status     # report running/stopped + pid
#
# Configure via environment variables or edit the defaults below:
#   INKFLOW_DIR    directory containing the binary/config/pid/log (default: script's own directory)
#   INKFLOW_BIN    path to the binary (default: $INKFLOW_DIR/inkflow-synology-ds918)
#   INKFLOW_CONFIG path to the TOML config (default: $INKFLOW_DIR/inkflow.toml)
#
# Wire this into DSM Task Scheduler as a "User-defined script" (triggered
# manually or on boot) to control the service without SSH each time.

set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
INKFLOW_DIR="${INKFLOW_DIR:-$SCRIPT_DIR}"
INKFLOW_BIN="${INKFLOW_BIN:-$INKFLOW_DIR/inkflow-synology-ds918}"
INKFLOW_CONFIG="${INKFLOW_CONFIG:-$INKFLOW_DIR/inkflow.toml}"
PID_FILE="$INKFLOW_DIR/inkflow.pid"
LOG_FILE="$INKFLOW_DIR/inkflow.log"

is_running() {
    [ -f "$PID_FILE" ] || return 1
    pid="$(cat "$PID_FILE" 2>/dev/null || true)"
    [ -n "$pid" ] || return 1
    kill -0 "$pid" 2>/dev/null
}

do_start() {
    if is_running; then
        pid="$(cat "$PID_FILE")"
        echo "inkflow already running (pid $pid)"
        return 0
    fi
    if [ ! -x "$INKFLOW_BIN" ]; then
        echo "error: binary not found or not executable: $INKFLOW_BIN" >&2
        exit 1
    fi
    if [ ! -f "$INKFLOW_CONFIG" ]; then
        echo "error: config not found: $INKFLOW_CONFIG" >&2
        exit 1
    fi
    echo "starting inkflow..."
    nohup "$INKFLOW_BIN" --config "$INKFLOW_CONFIG" serve >>"$LOG_FILE" 2>&1 &
    echo "$!" > "$PID_FILE"
    sleep 1
    if is_running; then
        echo "started (pid $(cat "$PID_FILE"))"
    else
        echo "error: inkflow exited immediately — check $LOG_FILE" >&2
        rm -f "$PID_FILE"
        exit 1
    fi
}

do_stop() {
    if ! is_running; then
        echo "inkflow not running"
        rm -f "$PID_FILE"
        return 0
    fi
    pid="$(cat "$PID_FILE")"
    echo "stopping inkflow (pid $pid)..."
    kill "$pid" 2>/dev/null || true
    i=0
    while kill -0 "$pid" 2>/dev/null; do
        i=$((i + 1))
        if [ "$i" -ge 20 ]; then
            echo "still running after 10s, sending SIGKILL"
            kill -9 "$pid" 2>/dev/null || true
            break
        fi
        sleep 0.5
    done
    rm -f "$PID_FILE"
    echo "stopped"
}

do_status() {
    if is_running; then
        echo "inkflow running (pid $(cat "$PID_FILE"))"
    else
        echo "inkflow stopped"
    fi
}

case "${1:-}" in
    start) do_start ;;
    stop) do_stop ;;
    restart)
        do_stop
        do_start
        ;;
    status) do_status ;;
    *)
        echo "usage: $0 {start|stop|restart|status}" >&2
        exit 1
        ;;
esac
