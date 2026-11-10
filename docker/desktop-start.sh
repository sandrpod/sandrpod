#!/bin/sh
# 桌面沙箱入口：起 KasmVNC（X + Web 客户端）→ XFCE 会话 → toolbox。
# 密码、xstartup、桌面选择标记已在镜像里烘焙（.kasmpasswd / xstartup /
# .de-was-selected），这里只负责按顺序拉起。
set -x
export HOME=/root
export DISPLAY=:1
# 容器重启后残留的 X 锁会让 KasmVNC 拒绝启动——清掉再起
rm -f /tmp/.X1-lock /tmp/.X11-unix/X1 2>/dev/null
# 上一次容器里 Chrome 崩溃留下的单例锁会让下次启动变成 "Opening in existing
# browser session" 而看不到窗口，开机清掉。
rm -rf /root/.config/google-chrome/Singleton* 2>/dev/null

# toolbox 先在后台跑（execute/files/MCP 端口 8080）。
/app/toolbox -port 8080 &
TB=$!

# 1) Xvnc 直启：绕过 kasmvncserver 包装器的交互菜单，并关闭 websocket 的
#    basic auth（-DisableBasicAuth 必须带值 1；裸写会被忽略而仍强制认证）。
#    沙箱本身就是隔离边界，preview URL 是入口，不需要再叠一层 VNC 口令。
GEOM=$(cat /etc/sandrpod-desktop-geometry 2>/dev/null || echo 1280x800)
# FrameRate 30：60fps 对远程桌面是纯浪费，编码吃满 CPU 还看不清；
# ffmpeg 在镜像里时 KasmVNC 会用 H.264（比 webp 便宜一个数量级）。
/usr/bin/Xvnc :1 -SecurityTypes None -DisableBasicAuth 1 -websocketPort 6901 \
  -httpd /usr/share/kasmvnc/www -interface 0.0.0.0 -FrameRate 30 -videoCodec h264 \
  -geometry "$GEOM" -depth 24 >/tmp/xvnc.log 2>&1 &
# 等 X 真正可用：检测 socket 而不是调 xdpyinfo（镜像里没有 x11-utils，
# 那个循环会白等 50 秒并在日志里刷屏）。30 秒还没起来就继续，桌面会话
# 起不来时下面的 wait 会把容器带下去，重启策略接手。
i=0
while [ $i -lt 30 ] && [ ! -S /tmp/.X11-unix/X1 ]; do
  i=$((i+1))
  sleep 1
done
[ -S /tmp/.X11-unix/X1 ] || echo "warning: /tmp/.X11-unix/X1 not present after ${i}s" >&2

# 2) 桌面会话（XFCE；xstartup 已烘焙，二选一即可）
# 关掉窗口合成：VNC 下重绘越少越流畅
xfconf-query -c xfwm4 -p /general/use_compositing -s false >/dev/null 2>&1 || true
dbus-launch --exit-with-session startxfce4 &
XFC=$!

# 3) 桌面栈挂了就退出容器（重启策略负责拉起）
wait "$XFC" 2>/dev/null || wait
