#!/usr/bin/env bash
# mydocker.sh — «свой Docker» из частей 2–4: namespaces + cgroup + capabilities + seccomp.
#
#   ./mydocker.sh run  [name]          запустить api в контейнере (в foreground, Ctrl+C — стоп)
#   ./mydocker.sh exec [name] cmd...   выполнить команду внутри (как docker exec)
#
# Лимиты задаются переменными: MEM=100M CPU="50000 100000" PIDS=50
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
CMD=${1:-}; NAME=${2:-box}
CG=/sys/fs/cgroup/mydocker-$NAME

MEM=${MEM:-100M}
CPU=${CPU:-"50000 100000"}
PIDS=${PIDS:-50}

# PID процесса api на хосте — ищем его среди процессов cgroup.
host_pid() {
  for p in $(cat "$CG/cgroup.procs" 2>/dev/null); do
    [ "$(cat /proc/$p/comm 2>/dev/null)" = api ] && { echo "$p"; return; }
  done
  echo "mydocker: контейнер $NAME не запущен" >&2; exit 1
}

case "$CMD" in
run)
  # 1. cgroup: лимиты памяти, CPU, числа процессов (часть 3)
  sudo mkdir -p "$CG"
  echo "$MEM"  | sudo tee "$CG/memory.max"      >/dev/null
  echo 0       | sudo tee "$CG/memory.swap.max" >/dev/null
  echo "$CPU"  | sudo tee "$CG/cpu.max"         >/dev/null
  echo "$PIDS" | sudo tee "$CG/pids.max"        >/dev/null
  trap 'sudo rmdir "$CG" 2>/dev/null || true' EXIT

  # seccomp-профиль из JSON (тот же, что для docker run) → BPF для setpriv
  [ "$DIR/seccomp.bpf" -nt "$DIR/seccomp.json" ] || "$DIR/seccompgen/seccompgen" "$DIR/seccomp.json" "$DIR/seccomp.bpf"

  echo "mydocker: $NAME  mem=$MEM cpu='$CPU' pids=$PIDS"
  # 2. Шелл кладёт себя в cgroup, дальше всё наследуется детьми.
  # 3. unshare — свои namespaces (часть 2), root внутри = мой uid снаружи.
  # 4. setpriv — последний шаг перед api: пустой bounding set + no_new_privs + seccomp (часть 4).
  bash -c "
    echo \$\$ | sudo tee '$CG/cgroup.procs' >/dev/null
    exec unshare --user --map-root-user --pid --fork --mount-proc --net --uts --ipc \
      bash -c 'hostname $NAME; ip link set lo up; cd \"$DIR/api\"
               exec setpriv --bounding-set=-all --inh-caps=-all --no-new-privs \
                            --seccomp-filter \"$DIR/seccomp.bpf\" ./api'
  "
  ;;
exec)
  shift 2
  exec nsenter -t "$(host_pid)" -U -p -m -n -u -i --preserve-credentials "${@:-bash}"
  ;;
*)
  sed -n '2,8p' "$0"; exit 2
  ;;
esac
