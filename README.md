# whisper-stt

Hold a hotkey → speak → release → text appears in your clipboard or the active window.

Runs **fully locally** using OpenAI's open-source Whisper model. No API key, no cloud.

---

## Requirements

### System packages (Debian/Ubuntu)

```bash
sudo apt install \
    libx11-xcb1-dev \
    libxkbcommon-dev \
    libxkbcommon-x11-dev \
    libxext-dev \
    libxtst-dev \
    portaudio19-dev \
    xdotool \
    ffmpeg
```

- **gohook** (X11 deps: `libx11-xcb1-dev`, `libxkbcommon-dev`, `libxkbcommon-x11-dev`, `libxext-dev`, `libxtst-dev`) — global hotkey capture
- **portaudio** (`portaudio19-dev`) — audio capture
- **xdotool** — typing text into the active window (only for `--type-direct` mode)
- **ffmpeg** — audio decoding for Whisper

### Go

```bash
# Install Go (if you haven't)
curl -Lo go.tar.gz https://go.dev/dl/go1.22.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go.tar.gz
export PATH=$PATH:/usr/local/go/bin   # add to ~/.bashrc or ~/.zshrc
```

---

## Installation

```bash
git clone https://github.com/shearn89/whisper-stt
cd whisper-stt
./install.sh
```

The install script will:
1. Install system dependencies (may prompt for sudo password)
2. Build the Go binary
3. Install it to `~/.local/bin/whisper-stt`
4. Copy the default config to `~/.config/whisper-stt/config.toml`
5. Install the systemd user service

---

## Configuration

Edit `~/.config/whisper-stt/config.toml`:

```toml
# Whisper model size: tiny, base, small, medium, large
model = "base"

# Whisper binary: "whisper" (openai-whisper pip package) or "whisper-cli" (whisper.cpp)
whisper_bin = "whisper"

# Hotkey (pynput syntax)
hotkey = "<ctrl>+<alt>+r"

# Trigger mode: "hold" (record while held) or "toggle" (press to start/stop)
trigger = "hold"

# Type into active window instead of clipboard
type_direct = false

# Send Enter after delivering text
send_enter = false

# Language (empty = auto-detect)
language = ""

# Audio device index (-1 = default)
device = -1
```

### Getting a Whisper binary

**Option A — Python openai-whisper (recommended):**

```bash
pip install openai-whisper
```

**Option B — whisper.cpp CLI:**

```bash
git clone https://github.com/ggerganov/whisper.cpp
cd whisper.cpp
mkdir build && cd build
cmake ..
make -j4
cp ../build/bin/whisper-cli ~/.local/bin/whisper-cli
```

Update `whisper_bin = "whisper-cli"` in config.toml if using whisper.cpp.

---

## Usage

```bash
# Run directly
whisper-stt

# Or use the systemd service
systemctl --user enable --now whisper-stt.service
systemctl --user status whisper-stt.service
journalctl --user -u whisper-stt -f
```

### CLI flags

```
--model      {tiny,base,small,medium,large}   Model size (default: base)
--hotkey     KEY COMBO                        Trigger combo (default: <ctrl>+<alt>+r)
--trigger    {hold,toggle}                    hold = record while held, toggle = press once/press again
--type-direct                                 Type into active window instead of clipboard
--send-enter                                  Press Enter after delivering text
--language   LANG                             Force language (e.g. en). Auto-detect if omitted.
--device     INT                              PortAudio input device index
--list-devices                               List available audio devices
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

## Wayland note

Global hotkey capture requires X11/XWayland. On a pure Wayland compositor:

- **KDE Plasma**: works under XWayland
- **GNOME**: may need `xdg-desktop-portal` + `atspi` backend — set `AT_SPI_BUS_TYPE=dbus`

Alternatively, bind a custom shortcut in your compositor that runs `whisper-stt --trigger toggle` in a terminal.

---

## Uninstall

```bash
./install.sh uninstall
```

This removes the binary, service file, and reloads systemd. Your config at `~/.config/whisper-stt/` is left intact.
