#!/bin/bash
cd "$(dirname "$0")"

PIDFILE="/tmp/geminigo.pid"

case "${1:-start}" in
  start)
    if [ -f "$PIDFILE" ] && kill -0 "$(cat $PIDFILE)" 2>/dev/null; then
      echo "Already running (PID $(cat $PIDFILE))"
      exit 0
    fi
    setsid ./geminigo --config config.json </dev/null &>/tmp/geminigo.log &
    echo $! > "$PIDFILE"
    sleep 1
    if ss -tlnp | grep -q 8081; then
      echo "Server started on http://localhost:8081"
    else
      echo "Failed to start"
      exit 1
    fi
    ;;
  stop)
    if [ -f "$PIDFILE" ]; then
      kill "$(cat $PIDFILE)" 2>/dev/null
      rm -f "$PIDFILE"
      echo "Stopped"
    else
      echo "Not running"
    fi
    ;;
  restart)
    $0 stop
    sleep 1
    $0 start
    ;;
  status)
    if [ -f "$PIDFILE" ] && kill -0 "$(cat $PIDFILE)" 2>/dev/null; then
      echo "Running (PID $(cat $PIDFILE))"
    else
      echo "Not running"
    fi
    ;;
  log)
    tail -f /tmp/geminigo.log
    ;;
  *)
    echo "Usage: $0 {start|stop|restart|status|log}"
    ;;
esac
