#!/system/bin/sh
#######################################
# 文件: customize.sh
# 功能: Android SSH Server 模块安装脚本
# 用法: 由管理器在安装模块时自动调用
# 依赖: Magisk/KernelSU/APatch 标准安装环境
#######################################

ui_print "正在安装 Android SSH Server..."
set_perm_recursive "$MODPATH" 0 0 0755 0644
set_perm "$MODPATH/service.sh" 0 0 0755
set_perm "$MODPATH/bin/sshd-server" 0 0 0755

RUNTIME=/data/adb/android-sshd
mkdir -p "$RUNTIME"
chmod 700 "$RUNTIME"

# 安装时生成随机密码并写入运行态与日志（adb shell su -c 可查看）
# 不使用固定默认密码；公钥由用户在首次密码登录后自行写入 authorized_keys
if [ ! -f "$RUNTIME/sshd.conf" ] || ! grep -q '^PASSWORD=.' "$RUNTIME/sshd.conf" 2>/dev/null; then
  PW=$(dd if=/dev/urandom bs=8 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n')
  if [ -z "$PW" ]; then
    PW=$(date +%s%N 2>/dev/null || date +%s)
  fi
  {
    echo "# Android SSH Server 配置（修改后执行: sh /data/adb/android-sshd/restart.sh restart）"
    echo "# 仅允许来源网段 ALLOW_NET 内的连接（默认 192.168.0.0/24）"
    echo "# 该 SSH 以 root 运行。密码为安装随机生成，请通过 adb shell 查看 password.txt"
    echo "USERNAME=root"
    echo "PASSWORD=$PW"
    echo "SSH_PORT=2222"
    echo "ALLOW_NET=192.168.0.0/24"
  } > "$RUNTIME/sshd.conf"
  chmod 600 "$RUNTIME/sshd.conf"
  printf '%s' "$PW" > "$RUNTIME/password.txt"
  chmod 600 "$RUNTIME/password.txt"
  echo "[install] 已生成随机 SSH 密码，查看: adb shell su -c \"cat $RUNTIME/password.txt\"" >> "$RUNTIME/sshd.log"
  echo "[install] USERNAME=root SSH_PORT=2222 ALLOW_NET=192.168.0.0/24" >> "$RUNTIME/sshd.log"
  echo "[install] 首次请用密码连接，再将公钥追加到 $RUNTIME/authorized_keys" >> "$RUNTIME/sshd.log"
  ui_print "已生成随机密码（未使用默认口令）"
  ui_print "查看密码: adb shell su -c \"cat /data/adb/android-sshd/password.txt\""
else
  ui_print "已保留现有配置与密码: /data/adb/android-sshd/sshd.conf"
fi

touch "$RUNTIME/authorized_keys" 2>/dev/null
chmod 600 "$RUNTIME/authorized_keys" 2>/dev/null

ui_print "安装完成，重启后生效"
ui_print "SSH: root@<设备IP> -p 2222  仅允许来源 192.168.0.*"
ui_print "首次密码登录后请自行配置公钥: $RUNTIME/authorized_keys"
