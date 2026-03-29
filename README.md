# whisper-stt

Hold a hotkey → speak → release → text appears in your clipboard or the active window.

Runs **fully locally** using OpenAI's open-source Whisper model. No API key, no cloud.

---

## Requirements

### System packages

```bash
# Debian/Ubuntu
sudo apt install xdotool portaudio19-dev ffmpeg

# Arch
sudo paru -S xdotool portaudio ffmpeg
```

- **xdotool** — only needed for `--type-direct` mode
- **portaudio** — required by sounddevice for audio capture
- **ffmpeg** — required by Whisper for audio decoding

### Python toolchain

```bash
# Install uv (if you haven't)
curl -LsSf https://astral.sh/uv/install.sh | sh

# Install mise (optional, for pinning Python version)
curl https://mise.run | sh
```

---

## Installation

```bash
git clone https://github.com/shearn89/whisper-stt
cd whisper-stt

# Create venv and install deps
uv sync

# Verify it works
uv run whisper-stt --help
```

The first run will download the Whisper model weights (~150 MB for `base`).

---

## Usage

```
uv run whisper-stt [OPTIONS]

Options:
  --model      {tiny,base,small,medium,large}   Model size (default: base)
  --hotkey     KEY COMBO                        Trigger combo (default: <ctrl>+<alt>+r)
  --trigger    {hold,toggle}                    hold = record while held, toggle = press once/press again
  --type-direct                                 Type into active window instead of clipboard
  --send-enter                                  Press Enter after delivering text
  --language   LANG                             Force language (e.g. en). Auto-detect if omitted.
  --device     INT                              sounddevice input device index
```

### Examples

```bash
# Default: hold Ctrl+Alt+R to record, releases to clipboard
uv run whisper-stt

# Larger model, toggle mode
uv run whisper-stt --model small --trigger toggle --hotkey "<ctrl>+<alt>+r"

# Type directly into active window and press Enter (e.g. for dictating into a terminal)
uv run whisper-stt --type-direct --send-enter

# Custom hotkey
uv run whisper-stt --hotkey "<super>+r"

# List audio devices to find your mic index
python -m sounddevice
uv run whisper-stt --device 2
```

### Hotkey syntax (pynput)

| Key          | Syntax          |
|--------------|-----------------|
| Ctrl         | `<ctrl>`        |
| Alt          | `<alt>`         |
| Super/Win    | `<super>`       |
| Shift        | `<shift>`       |
| Regular key  | `r`, `space`    |
| Combo        | `<ctrl>+<alt>+r`|

---

## Model sizes

| Model  | VRAM  | Relative speed | English WER |
|--------|-------|----------------|-------------|
| tiny   | ~1 GB | ~32x           | ~5.7%       |
| base   | ~1 GB | ~16x           | ~4.2%       |
| small  | ~2 GB | ~6x            | ~3.0%       |
| medium | ~5 GB | ~2x            | ~2.4%       |
| large  | ~10 GB| 1x             | ~2.1%       |

`base` is a good default. `small` is noticeably more accurate and still fast on most machines.

---

## Running as a background daemon (systemd user service)

```bash
# Copy the service file
mkdir -p ~/.config/systemd/user
cp whisper-stt.service ~/.config/systemd/user/

# Edit ExecStart to point at your installation path, then:
systemctl --user daemon-reload
systemctl --user enable --now whisper-stt

# Logs
journalctl --user -u whisper-stt -f
```

---

## Wayland note

`pynput` global hotkeys require X11/XWayland. On a pure Wayland compositor:

- **KDE Plasma**: works under XWayland
- **GNOME**: may need `xdg-desktop-portal` + `atspi` backend — set `AT_SPI_BUS_TYPE=dbus`
- As a workaround you can bind a custom shortcut in your compositor that runs `whisper-stt --trigger toggle` in a shell, removing the need for global hotkey capture entirely.

---

## Wake word (optional, advanced)

Picovoice Porcupine supports free wake-word detection but requires a (free) API key.
If you want this instead of a hotkey, open an issue or PR — the architecture supports it
as an alternative trigger via a separate thread calling `start_recording()`.
