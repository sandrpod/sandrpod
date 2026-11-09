#!/bin/sh
# 桌面沙箱入口：起 KasmVNC（X + Web 客户端）→ XFCE 会话 → toolbox。
# 密码、xstartup、桌面选择标记已在镜像里烘焙（.kasmpasswd / xstartup /
# .de-was-selected），这里只负责按顺序拉起。
set -x
export HOME=/root
export DISPLAY=:1
# 容器重启后残留的 X 锁会让 KasmVNC 拒绝启动——清掉再起
rm -f /tmp/.X1-lock /tmp/.X11-unix/X1 2>/dev/null

# toolbox 先在后台跑（execute/files/MCP 端口 8080）。
/app/toolbox -port 8080 &
TB=$!

# 1) Xvnc 直启：绕过 kasmvncserver 包装器的交互菜单，并关闭 websocket 的
#    basic auth（-DisableBasicAuth 必须带值 1；裸写会被忽略而仍强制认证）。
#    沙箱本身就是隔离边界，preview URL 是入口，不需要再叠一层 VNC 口令。
/usr/bin/Xvnc :1 -SecurityTypes None -DisableBasicAuth 1 -websocketPort 6901 \
  -httpd /usr/share/kasmvnc/www -interface 0.0.0.0 \
  -geometry 1280x800 -depth 24 >/tmp/xvnc.log 2>&1 &
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
