# Recorded on a machine with NO cached sandrpod images and NO leftover volume:
#   docker compose down -v && docker rmi ghcr.io/sandrpod/{server,poder,toolbox}:latest
# Anything less records a warm start, and the timings stop being true.
#
# Waits are on output, not on a stopwatch — the GIF advances the moment each
# step finishes, so it shows the real duration without the dead air.

Output demo.gif
Set FontSize 18
Set Width 1100
Set Height 700
Set Theme "Catppuccin Mocha"
Set TypingSpeed 45ms
Set Padding 20

Type `curl -O https://raw.githubusercontent.com/sandrpod/sandrpod/main/docker/docker-compose.yml`
Enter
Wait+Screen@30s /100\s+2192/
Sleep 800ms

Type `docker compose up -d`
Enter
Wait+Screen@180s /sandrpod-poder\s+Started/
Sleep 800ms

Type `sandrpod-cli --api-url http://localhost:8080 create demo --provider local`
Enter
Wait+Screen@180s /is ready/
Sleep 800ms

Type `sandrpod-cli --api-url http://localhost:8080 execute demo "echo hello from SandrPod; python3 -c 'print(6*7)'"`
Enter
# execute answers in under two seconds; a fixed beat is safer here than a
# regexp, because the screen pads every line with trailing spaces.
Sleep 4s
