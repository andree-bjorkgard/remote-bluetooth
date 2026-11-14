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
- Enter on a device connects or disconnects it (here if paired here, else on
  the machine that has it); Ctrl+Enter opens its own screen with the actions
  for it: connect or disconnect on each machine, "Play here" when the
  headphones are connected here but another machine is holding the audio, and
  "Play on <other>" when the sound plays here and the other machine is
  connected too.
- "Play here" / "Play on" are one-shot nudges, not modes: one machine lets go
  for a moment, the headphones switch, and it goes back to normal by itself.

Look: it must read as the existing rofi theme (~/.config/rofi/styling/
media.rasi + colors.rasi): gruvbox dark, bg #282828, fg #ebdbb2, accent
#83a598 for the selected row with pure black text, red #EC7875 for errors,
yellow for the in-progress message line; no green in rows. Devices not
connected on this computer are grey (#928374). Row second line: bold status,
detail, then battery. DejaVu Sans, 22 px prompt; 54 px Papirus device icons;
action icons are dark #3c3836 circles with an #ebdbb2 glyph (the app's
icons/*.svg); 500 px wide window with 30 px padding; rows with 4 px radius.
