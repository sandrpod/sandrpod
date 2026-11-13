#!/bin/sh
# 桌面沙箱入口：起 KasmVNC（X + Web 客户端）→ XFCE 会话 → toolbox。
# 密码、xstartup、桌面选择标记已在镜像里烘焙（.kasmpasswd / xstartup /
# .de-was-selected），这里只负责按顺序拉起。
set -x
export HOME=/root
export DISPLAY=:1
# 容器重启时 /tmp 还是上一次的：X 锁、dbus 会话 socket、xfce 会话缓存都会让
# 新会话立刻退出（表现为「停止后再启动，容器起来就死」）。开机清干净。
rm -f /tmp/.X1-lock /tmp/.X11-unix/X1 2>/dev/null
rm -rf /tmp/dbus-* /run/dbus/pid /root/.cache/sessions 2>/dev/null
# 上一次容器里 Chrome 崩溃留下的单例锁会让下次启动变成 "Opening in existing
# browser session" 而看不到窗口，开机清掉。
rm -rf /root/.config/google-chrome/Singleton* 2>/dev/null

# 0) 用户数据持久化：桌面沙箱的 /workspace 是卷（sandbox 停止/启动、容器重建都不丢）。
#    把用户可见的目录 + Chrome profile 落进卷里，做到"关掉再打开，标签页和下载还在"。
#    只搬用户数据目录：/root/.config 留着镜像里的烘焙配置（主题/壁纸），不被空卷遮住。
for d in Desktop Downloads Documents; do
  mkdir -p "/workspace/.home/$d"
  if [ -d "/root/$d" ] && [ ! -L "/root/$d" ]; then
    cp -a "/root/$d/." "/workspace/.home/$d/" 2>/dev/null || true
    rm -rf "/root/$d"
  fi
  ln -sfn "/workspace/.home/$d" "/root/$d"
done
mkdir -p /workspace/.home/chrome-profile
if [ ! -L /root/.chrome-profile ]; then
  if [ -d /root/.chrome-profile ]; then
    cp -a /root/.chrome-profile/. /workspace/.home/chrome-profile/ 2>/dev/null || true
    rm -rf /root/.chrome-profile
  fi
  ln -sfn /workspace/.home/chrome-profile /root/.chrome-profile
fi
# MCP 配置与 OAuth 令牌（~/.sandrpod：mcp.json、oauth/<server>.json）同样要在容器
# 重建后还在——否则换一次镜像就得把所有第三方服务重新授权一遍。令牌是 0600，
# cp -a 会保留权限，卷上的目录本身收紧到 0700。
mkdir -p /workspace/.home/.sandrpod
chmod 700 /workspace/.home/.sandrpod 2>/dev/null || true
if [ ! -L /root/.sandrpod ]; then
  if [ -d /root/.sandrpod ]; then
    cp -a /root/.sandrpod/. /workspace/.home/.sandrpod/ 2>/dev/null || true
    rm -rf /root/.sandrpod
  fi
  ln -sfn /workspace/.home/.sandrpod /root/.sandrpod
fi

# 持久化的 Chrome profile 里留着上一次容器的 SingletonLock（hostname-pid）。
# 重启后 hostname 变了，Chrome 判定「被另一台机器占用」并退出，界面看到的就是
# 「浏览器打不开」。清掉这三个文件让它重新拿锁。
rm -f /root/.chrome-profile/Singleton* 2>/dev/null

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
XVNC=$!
# 等 X 真正可用：检测 socket 而不是调 xdpyinfo（镜像里没有 x11-utils，
# 那个循环会白等 50 秒并在日志里刷屏）。30 秒还没起来就继续，桌面会话
# 起不来时下面的 wait 会把容器带下去，重启策略接手。
i=0
while [ $i -lt 30 ] && [ ! -S /tmp/.X11-unix/X1 ]; do
  i=$((i+1))
  sleep 1
done
[ -S /tmp/.X11-unix/X1 ] || echo "warning: /tmp/.X11-unix/X1 not present after ${i}s" >&2

# 2) 一个 dbus 会话，dock 设置和桌面会话共用它。
#    之前这里直接用 xfconf-query 却不开 dbus，命令静默失败（xfconf 没有会话可写），
#    所以底部 dock 从来没生效过。现在先起 bus、把它写进配置、再让 XFCE 读这份配置。
eval "$(dbus-launch --sh-syntax --exit-with-session)"
export DBUS_SESSION_BUS_ADDRESS

# mac 风 dock：镜像里放了 /etc/sandrpod-desktop-dock 时生效（DESKTOP_THEME=dock）。
if [ -f /etc/sandrpod-desktop-dock ]; then
  q() { xfconf-query -c "$1" -p "$2" -s "$3" 2>/dev/null || xfconf-query -c "$1" -p "$2" -n -t string -s "$3" 2>/dev/null || true; }
  # 面板：贴底、只占中间一段、图标大一点、锁住位置
  q xfce4-panel /panels/panel-1/position "p=8"
  q xfce4-panel /panels/panel-1/size "44"
  q xfce4-panel /panels/panel-1/length-adjust "false"
  q xfce4-panel /panels/panel-1/length "30"
  q xfce4-panel /panels/panel-1/position-locked "true"
  q xfce4-panel /panels/panel-1/autohide-behavior "0"
  q xfce4-panel /panels/panel-1/background-style "0"
  # 窗口按钮放左边（红黄绿的位置），mac 习惯
  q xfwm4 /general/button_layout "CMH|"
  # Dock 要圆角、无边框的观感：让面板用深色 alpha 底色
  q xfce4-panel /panels/panel-1/background-rgba "0.10,0.10,0.12,0.85" 2>/dev/null || true
fi

# 关掉窗口合成：VNC 下重绘越少越流畅
xfconf-query -c xfwm4 -p /general/use_compositing -s false >/dev/null 2>&1 || true

# 3) 容器寿命跟 X 服务器绑定：Xvnc 活着容器就活着，桌面会话由循环负责续。
#    Xvnc 死了说明图形栈本身坏了，退出容器让重启策略接手。
wait "$XVNC" 2>/dev/null || wait
