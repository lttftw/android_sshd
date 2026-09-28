#!/system/bin/sh
#######################################
# 文件: service.sh
# 功能: 开机启动 Android SSH Server
# 说明: 仅作为 Magisk/KernelSU/APatch 的开机触发器。
#       真正的启动/停止/重启/状态/改密等管理逻辑全部内置在 sshd-server CLI 中，
#       无需依赖本脚本，可在设备上直接执行: /data/adb/modules/android-sshd/bin/sshd-server <命令>
#######################################

MODDIR=${0%/*}
RUNTIME=/data/adb/android-sshd
BIN="$MODDIR/bin/sshd-server"

[ -x "$BIN" ] || exit 0

# 幂等：初始化配置/密钥（缺失才生成随机密码），并以独立会话后台拉起守护进程。
"$BIN" -home "$RUNTIME" start
