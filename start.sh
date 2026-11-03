#!/usr/bin/env bash
set -e

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PID_FILE="$DIR/grok2api.pid"
LOG_FILE="$DIR/logs/grok2api.log"
BIN="$DIR/backend/grok2api"
CONFIG="$DIR/config.yaml"

# Outbound proxy targeting local Clash Verge
export HTTP_PROXY="http://127.0.0.1:7897"
export HTTPS_PROXY="http://127.0.0.1:7897"
export ALL_PROXY="socks5://127.0.0.1:7897"
export NO_PROXY="localhost,127.0.0.1,10.144.144.0/24,10.0.0.0/8"

start() {
    if [ -f "$PID_FILE" ]; then
        PID=$(cat "$PID_FILE")
        if kill -0 "$PID" 2>/dev/null; then
            echo "[*] grok2api is already running (PID: $PID)"
            return 0
        fi
        rm -f "$PID_FILE"
    fi

    echo "[+] Starting grok2api..."
    cd "$DIR"
    nohup "$BIN" --config "$CONFIG" >> "$LOG_FILE" 2>&1 &
    PID=$!
    echo "$PID" > "$PID_FILE"
    sleep 2

    if kill -0 "$PID" 2>/dev/null; then
        echo "[+] grok2api started successfully (PID: $PID)"
        echo "[+] Log file: $LOG_FILE"
        echo "[+] Web / API: http://127.0.0.1:8000"
    else
        echo "[-] grok2api failed to start. Last log lines:"
        tail -n 20 "$LOG_FILE"
        rm -f "$PID_FILE"
        exit 1
    fi
}

stop() {
    if [ -f "$PID_FILE" ]; then
        PID=$(cat "$PID_FILE")
        if kill -0 "$PID" 2>/dev/null; then
            echo "[*] Stopping grok2api (PID: $PID)..."
            kill "$PID"
            for i in {1..10}; do
                if ! kill -0 "$PID" 2>/dev/null; then
                    break
                fi
                sleep 0.5
            done
            if kill -0 "$PID" 2>/dev/null; then
                echo "[!] Process still running, forcing kill -9..."
                kill -9 "$PID" 2>/dev/null || true
            fi
            echo "[+] Stopped."
        else
            echo "[*] Process $PID is not running."
        fi
        rm -f "$PID_FILE"
    else
        echo "[*] No PID file found."
    fi
}

status() {
    if [ -f "$PID_FILE" ]; then
        PID=$(cat "$PID_FILE")
        if kill -0 "$PID" 2>/dev/null; then
            echo "[+] grok2api is running (PID: $PID)"
            return 0
        else
            echo "[-] PID file exists ($PID) but process is not running"
            return 1
        fi
    else
        echo "[-] grok2api is stopped"
        return 3
    fi
}

case "${1:-start}" in
    start)
        start
        ;;
    stop)
        stop
        ;;
    restart)
        stop
        sleep 1
        start
        ;;
    status)
        status
        ;;
    *)
        echo "Usage: $0 {start|stop|restart|status}"
        exit 1
        ;;
esac
