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

# 2) 桌面会话（XFCE）。会话进程偶尔会自己退出（重启后的旧 dbus 状态、XFCE 崩溃），
#    以前这会让脚本跟着退出、容器直接死掉，沙箱就再也起不来。改成自愈循环：
#    XFCE 死了重启它，容器只在 Xvnc 死时退出。
# 关掉窗口合成：VNC 下重绘越少越流畅
xfconf-query -c xfwm4 -p /general/use_compositing -s false >/dev/null 2>&1 || true
# Dock 布局必须在"面板已经存在"之后施加：手写 xfce4-panel.xml 会被面板启动时的
# 首次保存覆盖（实测面板仍在顶部），而在会话外跑 xfconf-query 没有会话总线可用
# （XFCONF 静默失败）。所以：起会话 → 等面板起来 → 用同一个总线改设置 → 重启面板。
apply_dock_layout() {
  [ -f /etc/sandrpod-desktop-dock ] || return 0
  i=0
  while [ $i -lt 20 ]; do
    if xfconf-query -c xfce4-panel -l 2>/dev/null | grep -q panels; then break; fi
    i=$((i+1)); sleep 1
  done
  # 位置按实际分辨率算：面板占 22% 宽，居中放置。写死坐标在 1024/1920 上都不居中。
  GEOM=$(cat /etc/sandrpod-desktop-geometry 2>/dev/null || echo 1280x800)
  W=${GEOM%x*}
  LEN=$((W * 22 / 100))
  X=$(( (W - LEN) / 2 ))
  H=$(cat /etc/sandrpod-desktop-geometry 2>/dev/null | cut -dx -f2)
  Y=$(( H - 48 ))
  # x 是面板中心的坐标，不是左边缘；KasmVNC 会把虚拟屏幕改成浏览器窗口大小，
  # 所以按当前宽度算中心，而不是写死。
  panel_center() {
    W=$(DISPLAY=:1 xrandr --current 2>/dev/null | sed -n 's/.*current \([0-9]*\) x \([0-9]*\).*/\1/p' | head -1)
    [ -n "$W" ] || W=$(echo "${GEOM%x*}")
    H=$(DISPLAY=:1 xrandr --current 2>/dev/null | sed -n 's/.*current \([0-9]*\) x \([0-9]*\).*/\2/p' | head -1)
    [ -n "$H" ] || H=$(echo "${GEOM#*x}")
    echo "$((W / 2)) $((H - 48))"
  }
  read -r CX CY <<EOF_RD
$(panel_center)
EOF_RD
  xfconf-query -c xfce4-panel -p /panels/panel-1/position -s "p=0;x=$CX;y=$CY" 2>/dev/null || true
  xfconf-query -c xfce4-panel -p /panels/panel-1/size -s 48 2>/dev/null || true
  xfconf-query -c xfce4-panel -p /panels/panel-1/length-adjust -s false 2>/dev/null || true
  xfconf-query -c xfce4-panel -p /panels/panel-1/length -s 22 2>/dev/null || true
  xfconf-query -c xfce4-panel -p /panels/panel-1/position-locked -s true 2>/dev/null || true
  xfconf-query -c xfwm4 -p /general/button_layout -s "CMH|" 2>/dev/null || true
  # 壁纸：属性名按显示器型号命名（monitor0 / Virtual-1 / default），逐个设；
  # 光写一份 XML 在名称不匹配时会被忽略——这就是"壁纸一直是老鼠"的原因。
  WALL=$(cat /etc/sandrpod-desktop-wallpaper 2>/dev/null)
  if [ -n "$WALL" ] && [ -f "$WALL" ]; then
    for prop in $(xfconf-query -c xfce4-desktop -l 2>/dev/null | grep -E 'last-image|image-path'); do
      xfconf-query -c xfce4-desktop -p "$prop" -s "$WALL" 2>/dev/null || true
    done
    xfdesktop --reload >/dev/null 2>&1 || true
  fi
  # 面板让位给 plank（自动隐藏，不占屏幕边缘）
  xfconf-query -c xfce4-panel -p /panels/panel-1/autohide-behavior -s 2 2>/dev/null || true
  xfce4-panel -r >/dev/null 2>&1 || true
  # plank 才是 dock 本体：居中、透明、图标放大
  if command -v plank >/dev/null 2>&1; then
    pgrep -x plank >/dev/null 2>&1 || (plank >/tmp/plank.log 2>&1 &)
  fi
}
(
  while true; do
    eval "$(dbus-launch --sh-syntax --exit-with-session)"
    export DBUS_SESSION_BUS_ADDRESS
    startxfce4 &
    XFCE_PID=$!
    apply_dock_layout
    wait "$XFCE_PID"
    echo "desktop session exited, restarting in 2s" >&2
    sleep 2
  done
) &
XFC=$!

# 3) 预开一个 Chrome：CDP（9222）是 browser-use / Playwright MCP 的入口，
#    agent 连过来时浏览器必须已经在跑。失败不阻塞桌面。
if [ -x /usr/bin/google-chrome ]; then
  sleep 3
  DISPLAY=:1 dbus-launch --exit-with-session google-chrome     --no-first-run --no-default-browser-check about:blank >/tmp/chrome.log 2>&1 &
fi

# 3) 容器寿命跟 X 服务器绑定：Xvnc 活着容器就活着，桌面会话由循环负责续。
#    Xvnc 死了说明图形栈本身坏了，退出容器让重启策略接手。
wait "$XVNC" 2>/dev/null || wait
