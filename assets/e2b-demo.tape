# The unmodified E2B SDK against a self-hosted SandrPod deployment.
# Recorded live against https://api.e2b.sandrpod.com — the sandbox is real,
# created and destroyed during the take.
#
# E2B_API_KEY is set inside a Hide block: it is a credential, not part of the
# demonstration. Everything the viewer needs to reproduce this is on screen.

Output e2b.gif
Set FontSize 16
Set Width 1100
Set Height 780
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms
Set Padding 20

Hide
# Set up off-camera: a venv with `pip install e2b-code-interpreter`, and
# E2B_API_KEY exported. Run this tape from the directory holding e2b-demo.py.
Type `source .venv/bin/activate && export E2B_API_KEY=$YOUR_KEY && clear`
Enter
Sleep 2s
Show

Type `cat e2b-demo.py`
Enter
Sleep 3s

Type `export E2B_DOMAIN=e2b.sandrpod.com`
Enter
Sleep 1500ms

Type `python e2b-demo.py`
Enter
# Wait on a value that only the RUN can produce. "killed" also appears in the
# source listing above, so waiting for it matched before the script even ran.
Wait+Screen@90s /chart: [0-9]+ bytes/
Sleep 2500ms
