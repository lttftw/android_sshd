#!/system/bin/sh
#######################################
# 文件: service.sh
# 功能: 开机启动 Android SSH Server
# 用法: Magisk/KernelSU/APatch 开机自动执行；也可手动执行以立即启动
# 依赖: bin/sshd-server
#######################################

MODDIR=${0%/*}
RUNTIME=/data/adb/android-sshd
BIN="$MODDIR/bin/sshd-server"

mkdir -p "$RUNTIME"
chmod 700 "$RUNTIME"

# 停掉已有实例（含 PID 文件丢失的情况）
stop_sshd() {
  if [ -f "$RUNTIME/sshd.pid" ]; then
    old=$(cat "$RUNTIME/sshd.pid" 2>/dev/null || true)
    if [ -n "$old" ]; then
      kill "$old" 2>/dev/null || true
      # 等待退出，最多 ~2s
      i=0
      while [ $i -lt 8 ]; do
        kill -0 "$old" 2>/dev/null || break
        sleep 0.25
        i=$((i+1))
      done
      kill -9 "$old" 2>/dev/null || true
    fi
    rm -f "$RUNTIME/sshd.pid"
  fi
  # 兜底：按进程名清掉残留，避免端口占用导致新进程 bind 失败
  for p in $(ps -A 2>/dev/null | grep '[s]shd-server' | awk '{print $2}'); do
    kill "$p" 2>/dev/null || true
    sleep 0.2
    kill -9 "$p" 2>/dev/null || true
  done
}

# 生成 16 位十六进制随机密码
gen_password() {
  dd if=/dev/urandom bs=8 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'
}

# 首次初始化
if [ ! -f "$RUNTIME/sshd.conf" ]; then
  PW=$(gen_password)
  if [ -z "$PW" ]; then
    # 生成失败则不写固定口令：留空表示关闭密码登录，仅公钥可登录
    PW=""
  fi
  {
    echo "# Android SSH Server 配置"
    echo "# 改 USERNAME/PASSWORD 后通常无需重启（登录时热加载）；改端口/网段需 restart"
    echo "# 仅允许来源网段 ALLOW_NET 内的连接（默认 192.168.0.0/24）"
    echo "USERNAME=root"
    echo "PASSWORD=$PW"
    echo "SSH_PORT=2222"
    echo "ALLOW_NET=192.168.0.0/24"
  } > "$RUNTIME/sshd.conf"
  chmod 600 "$RUNTIME/sshd.conf"
  printf '%s' "$PW" > "$RUNTIME/password.txt"
  chmod 600 "$RUNTIME/password.txt"
  echo "[sshd-init] 已生成随机登录密码，查看: cat $RUNTIME/password.txt" >> "$RUNTIME/sshd.log"
  echo "[sshd-init] 首次连接用密码登录后，请写入 $RUNTIME/authorized_keys 配置公钥" >> "$RUNTIME/sshd.log"
fi

# 每次启动重建 restart.sh（绝对路径，避免 $0 相对路径踩坑）
cat > "$RUNTIME/restart.sh" <<'SSHD_RESTART_EOF'
#!/system/bin/sh
# 用法: sh /data/adb/android-sshd/restart.sh {restart|stop|status|password}
RUNTIME=/data/adb/android-sshd
MODDIR=/data/adb/modules/android-sshd
do_stop() {
  if [ -f "$RUNTIME/sshd.pid" ]; then
    pid=$(cat "$RUNTIME/sshd.pid" 2>/dev/null || true)
    if [ -n "$pid" ]; then
      kill "$pid" 2>/dev/null || true
      i=0
      while [ $i -lt 8 ]; do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.25
        i=$((i+1))
      done
      kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$RUNTIME/sshd.pid"
  fi
  for p in $(ps -A 2>/dev/null | grep '[s]shd-server' | awk '{print $2}'); do
    kill "$p" 2>/dev/null || true
    sleep 0.2
    kill -9 "$p" 2>/dev/null || true
  done
}
case "$1" in
  stop)
    do_stop
    echo "已停止"
    ;;
  status)
    if [ -f "$RUNTIME/sshd.pid" ] && kill -0 "$(cat "$RUNTIME/sshd.pid")" 2>/dev/null; then
      echo "运行中 (pid $(cat "$RUNTIME/sshd.pid"))"
    else
      echo "未运行"
    fi
    ;;
  password)
    if [ -f "$RUNTIME/password.txt" ]; then
      cat "$RUNTIME/password.txt"
      echo
    else
      echo "未找到 password.txt"
    fi
    ;;
  restart)
    # 关键：在 SSH 会话内 restart 时，do_stop 杀掉 sshd-server 会销毁 PTY，
    # 当前前台脚本常收到 SIGHUP 中途退出，导致 service.sh 没跑到。
    # 必须先 setsid 脱离会话，再由后台 worker 执行 stop+start。
    echo "已提交后台重启：约 1~2 秒后当前 SSH 会话会断开，属正常。请稍候再连接。"
    if command -v setsid >/dev/null 2>&1; then
      setsid /system/bin/sh /data/adb/android-sshd/restart.sh _worker \
        </dev/null >>"$RUNTIME/sshd.log" 2>&1 &
    else
      ( trap '' HUP
        /system/bin/sh /data/adb/android-sshd/restart.sh _worker \
          </dev/null >>"$RUNTIME/sshd.log" 2>&1 &
      )
    fi
    # 给 worker 一点调度时间；即使随后 PTY 被销毁，worker 已在独立会话
    sleep 0.2
    ;;
  _worker)
    echo "[restart-worker] start $(date 2>/dev/null || true)" >>"$RUNTIME/sshd.log"
    do_stop
    sleep 0.5
    /system/bin/sh /data/adb/modules/android-sshd/service.sh >>"$RUNTIME/sshd.log" 2>&1
    if [ -f "$RUNTIME/sshd.pid" ] && kill -0 "$(cat "$RUNTIME/sshd.pid")" 2>/dev/null; then
      echo "[restart-worker] ok pid=$(cat "$RUNTIME/sshd.pid")" >>"$RUNTIME/sshd.log"
    else
      echo "[restart-worker] FAILED see service log" >>"$RUNTIME/sshd.log"
    fi
    ;;
  *) echo "用法: sh /data/adb/android-sshd/restart.sh {restart|stop|status|password}";;
esac
SSHD_RESTART_EOF
chmod 755 "$RUNTIME/restart.sh"

# CRLF 兼容
if [ -f "$RUNTIME/sshd.conf" ]; then
  tr -d '\r' < "$RUNTIME/sshd.conf" > "$RUNTIME/sshd.conf.tmp" 2>/dev/null && mv "$RUNTIME/sshd.conf.tmp" "$RUNTIME/sshd.conf"
  chmod 600 "$RUNTIME/sshd.conf"
fi

# 安全读取配置（禁止 source）
USERNAME=root
PASSWORD=
SSH_PORT=2222
ALLOW_NET=192.168.0.0/24
while IFS= read -r line || [ -n "$line" ]; do
  line=$(printf '%s' "$line" | tr -d '\r')
  case "$line" in
    ''|'#'*) continue ;;
  esac
  key=${line%%=*}
  val=${line#*=}
  case "$key" in
    USERNAME) USERNAME=$val ;;
    PASSWORD) PASSWORD=$val ;;
    SSH_PORT) SSH_PORT=$val ;;
    ALLOW_NET) ALLOW_NET=$val ;;
  esac
done < "$RUNTIME/sshd.conf"

case "$SSH_PORT" in
  ''|*[!0-9]*) SSH_PORT=2222 ;;
esac

# password.txt 与配置保持同步（用户改 PASSWORD 后可从这里读）
if [ -n "$PASSWORD" ]; then
  printf '%s' "$PASSWORD" > "$RUNTIME/password.txt"
  chmod 600 "$RUNTIME/password.txt"
fi

stop_sshd

# 脱离当前会话启动（setsid/nohup），避免 restart 后随父 shell 被回收
LOG="$RUNTIME/sshd.log"
if command -v setsid >/dev/null 2>&1; then
  setsid "$BIN" \
    -listen "0.0.0.0:$SSH_PORT" \
    -allow "$ALLOW_NET" \
    -authorized-keys "$RUNTIME/authorized_keys" \
    -host-key "$RUNTIME/ssh_host_ed25519" \
    -config "$RUNTIME/sshd.conf" \
    >> "$LOG" 2>&1 < /dev/null &
elif command -v nohup >/dev/null 2>&1; then
  nohup "$BIN" \
    -listen "0.0.0.0:$SSH_PORT" \
    -allow "$ALLOW_NET" \
    -authorized-keys "$RUNTIME/authorized_keys" \
    -host-key "$RUNTIME/ssh_host_ed25519" \
    -config "$RUNTIME/sshd.conf" \
    >> "$LOG" 2>&1 < /dev/null &
else
  ( trap '' HUP
    "$BIN" \
      -listen "0.0.0.0:$SSH_PORT" \
      -allow "$ALLOW_NET" \
      -authorized-keys "$RUNTIME/authorized_keys" \
      -host-key "$RUNTIME/ssh_host_ed25519" \
      -config "$RUNTIME/sshd.conf" \
      >> "$LOG" 2>&1 < /dev/null &
  )
fi

echo $! > "$RUNTIME/sshd.pid"
chmod 600 "$RUNTIME/sshd.pid" 2>/dev/null

# 确认起来
sleep 0.4
if kill -0 "$(cat "$RUNTIME/sshd.pid" 2>/dev/null)" 2>/dev/null; then
  echo "[service] sshd-server started pid=$(cat "$RUNTIME/sshd.pid") port=$SSH_PORT allow=$ALLOW_NET user=$USERNAME" >> "$LOG"
else
  echo "[service] sshd-server failed to stay up, see $LOG" >> "$LOG"
fi
