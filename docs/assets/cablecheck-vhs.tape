# Rebuild from the current source, then compare two canonical example reports.
Output docs/assets/cablecheck-vhs.gif

Require make
Require less

Set Shell "bash"
Set FontSize 16
Set FontFamily "JetBrainsMono Nerd Font"
Set LetterSpacing 0
Set Width 1100
Set Height 660
Set Padding 18
Set Margin 18
Set MarginFill "#07111f"
Set BorderRadius 12
Set WindowBar Colorful
Set Framerate 30
Set TypingSpeed 18ms
Set CursorBlink false
Set Theme "Catppuccin Mocha"
Set LoopOffset 18%

Hide
Type "make build >/dev/null && clear"
Enter
Sleep 3s
Show

Type "./cablecheck compare examples/healthy/report.json examples/crc-errors/report.json | less -R"
Sleep 500ms
Enter
Wait+Screen@10s /CableCheck comparison/
Sleep 2s

Down@90ms 12
Sleep 2s

Type "G"
Sleep 3s
Type "q"
Sleep 1s
