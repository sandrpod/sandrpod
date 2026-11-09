#!/bin/sh
# 桌面沙箱入口：起 KasmVNC（X + Web 客户端）→ XFCE 会话 → toolbox。
# 密码、xstartup、桌面选择标记已在镜像里烘焙（.kasmpasswd / xstartup /
# .de-was-selected），这里只负责按顺序拉起。
set -x
export HOME=/root
export DISPLAY=:1

# toolbox 先在后台跑（execute/files/MCP 端口 8080）。
/app/toolbox -port 8080 &
TB=$!

# 1) KasmVNC：X 服务器 + Web 客户端一起起（websocket_port 6901 已固定在 yaml）
kasmvncserver :1
# 等 X 真正可用（xdpyinfo 来自 x11-utils）
i=0
while [ $i -lt 50 ]; do
  if xdpyinfo -display "$DISPLAY" >/dev/null 2>&1; then
    break
  fi
  i=$((i+1))
  sleep 1
done

# 2) 桌面会话（XFCE；xstartup 已烘焙，二选一即可）
dbus-launch --exit-with-session startxfce4 &
XFC=$!

# 3) 桌面栈挂了就退出容器（重启策略负责拉起）
wait "$XFC" 2>/dev/null || wait
