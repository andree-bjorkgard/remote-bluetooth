---
skills: app-ui
---
rofi-bt: the rofi-blocks Bluetooth menu, now aware of every machine running
remote-bluetooth (this desktop "andree" and the xps17 "ingenting").

Who: one person with two Linux machines and multipoint headphones
(ATH-M50xBT2) connected to both, plus keyboards and mice that live on one.

What it has to do:
- One row per physical device, merged across machines, so the same
  headphones never appear twice. The row says where the device is connected
  and, for audio, which machine is actually playing on it.
- Selecting a device opens its own screen with the actions for it: connect or
  disconnect on each machine, and "Play here" when the headphones are
  connected here but another machine is holding the audio.
- "Play here" is a one-shot nudge, not a mode: the other machine lets go for a
  moment, the headphones switch to this one, and the other goes back to
  normal by itself.

Look: it must read as the existing rofi theme (~/.config/rofi/styling/
media.rasi + colors.rasi): gruvbox dark, bg #282828, fg #ebdbb2, accent
#83a598 for the selected row, green #61C766 for "on", red #EC7875 for errors;
DejaVu Sans, 22 px prompt, 12 px rows with a smaller grey second line; 54 px
Papirus icons; 500 px wide window with 30 px padding; rows with 4 px radius.
Keyboard driven: Return runs the row's action, Ctrl+Return its second action.
