# Run two real CableCheck peers with the repository's hermetic demo tools.
Output docs/assets/cablecheck-run.gif

Require make

Set Shell "bash"
Set FontSize 14
Set FontFamily "JetBrainsMono Nerd Font"
Set LetterSpacing 0
Set Width 1600
Set Height 700
Set Padding 18
Set Margin 18
Set MarginFill "#07111f"
Set BorderRadius 12
Set WindowBar Colorful
Set Framerate 15
Set PlaybackSpeed 3
Set TypingSpeed 12ms
Set CursorBlink false
Set Theme "Catppuccin Mocha"
Set LoopOffset 8%

Hide
Type "make build >/dev/null"
Enter
Sleep 3s
Type "export PATH=$PWD/testdata/stubtools:$PWD:$PATH"
Enter
Type "demo_dir=$(mktemp -d /tmp/cablecheck-vhs-demo.XXXXXX) && mkdir -p $demo_dir/pc1 $demo_dir/pc2"
Enter
Type `cleanup() { if [ -n "${worker:-}" ] && kill -0 "$worker" 2>/dev/null; then kill "$worker"; wait "$worker" || true; fi; rm -rf "$demo_dir"; }`
Enter
Type "trap cleanup EXIT INT TERM"
Enter
Type "cablecheck run --role pc2 --local-ip 127.0.0.2 --peer-ip 127.0.0.1 --mode quick --non-interactive --allow-virtual-interface --no-sudo --token demo-token --control-port 45200 --iperf-port 45201 --output $demo_dir/pc2 >$demo_dir/pc2.log 2>&1 & worker=$!"
Enter
Type "cd $demo_dir/pc1 && clear"
Enter
Sleep 1s
Show

Type `cablecheck run --role pc1 --local-ip 127.0.0.1 --peer-ip 127.0.0.2 \`
Enter
Type "  --mode quick --non-interactive --allow-virtual-interface --no-sudo --token demo-token --control-port 45200 --iperf-port 45201"
Sleep 300ms
Enter
Wait+Screen@45s /classification=INCONCLUSIVE/
Sleep 10s

Hide
Type "wait $worker || true; cleanup; trap - EXIT INT TERM"
Enter
Sleep 1s
