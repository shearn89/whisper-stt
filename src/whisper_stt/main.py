"""
whisper-stt: Hold a hotkey to record, release to transcribe.
"""

import argparse
import queue
import signal
import sys
import tempfile
import threading
import time
from pathlib import Path

import numpy as np
import pyperclip
import sounddevice as sd
import soundfile as sf
import whisper
from pynput import keyboard

# ---------------------------------------------------------------------------
# Globals
# ---------------------------------------------------------------------------

audio_queue: queue.Queue = queue.Queue()
recording_frames: list[np.ndarray] = []
is_recording = False
stop_event = threading.Event()
model: whisper.Whisper | None = None

SAMPLE_RATE = 16_000  # Whisper expects 16 kHz


# ---------------------------------------------------------------------------
# Audio helpers
# ---------------------------------------------------------------------------


def audio_callback(indata: np.ndarray, frames: int, time_info, status) -> None:
    """Called by sounddevice on each audio chunk."""
    if is_recording:
        recording_frames.append(indata.copy())


def start_recording() -> None:
    global is_recording, recording_frames
    recording_frames = []
    is_recording = True
    print("🎙  Recording…", flush=True)


def stop_and_transcribe(args: argparse.Namespace) -> str | None:
    global is_recording
    is_recording = False
    print("⏹  Stopped. Transcribing…", flush=True)

    if not recording_frames:
        print("⚠️  No audio captured.", flush=True)
        return None

    audio = np.concatenate(recording_frames, axis=0).flatten()

    # Write to a temp WAV so whisper can load it cleanly
    with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as tmp:
        tmp_path = Path(tmp.name)
        sf.write(tmp_path, audio, SAMPLE_RATE)

    try:
        result = model.transcribe(str(tmp_path), language=args.language or None)
        text: str = result["text"].strip()
    finally:
        tmp_path.unlink(missing_ok=True)

    if not text:
        print("⚠️  Nothing recognised.", flush=True)
        return None

    print(f"📝  {text}", flush=True)
    return text


def deliver_text(text: str, args: argparse.Namespace) -> None:
    """Put text in clipboard or type it directly."""
    if args.type_direct:
        _type_text(text, args.send_enter)
    else:
        pyperclip.copy(text)
        print("📋  Copied to clipboard.", flush=True)
        if args.send_enter:
            # Can't really "press Enter" in clipboard mode meaningfully,
            # but honour the flag by appending a newline so the user can paste and it lands.
            pyperclip.copy(text + "\n")


def _type_text(text: str, send_enter: bool) -> None:
    """Type text into the currently focused window using xdotool."""
    import shutil
    import subprocess

    if not shutil.which("xdotool"):
        print(
            "❌  xdotool not found. Install it: sudo apt install xdotool",
            file=sys.stderr,
        )
        pyperclip.copy(text)
        print("📋  Fell back to clipboard.", flush=True)
        return

    cmd = ["xdotool", "type", "--clearmodifiers", "--", text]
    subprocess.run(cmd, check=False)

    if send_enter:
        subprocess.run(["xdotool", "key", "Return"], check=False)

    print("⌨️  Typed into active window.", flush=True)


# ---------------------------------------------------------------------------
# Hotkey logic
# ---------------------------------------------------------------------------


def build_hotkey_listener(args: argparse.Namespace) -> keyboard.GlobalHotKeys | keyboard.Listener:
    """
    Two modes:
      - hold: press and hold a key combination; release triggers transcription.
      - toggle: one press starts, another stops.
    """
    combo = args.hotkey  # e.g. "<ctrl>+<alt>+r"

    if args.trigger == "toggle":
        return _build_toggle_listener(combo, args)
    else:
        return _build_hold_listener(combo, args)


def _parse_combo(combo: str) -> set[keyboard.Key | keyboard.KeyCode]:
    """Parse 'ctrl+alt+r' style combo into a set of pynput keys."""
    parts = [p.strip("<>") for p in combo.replace("+", " ").split()]
    keys: set[keyboard.Key | keyboard.KeyCode] = set()
    for p in parts:
        try:
            keys.add(keyboard.Key[p])
        except KeyError:
            keys.add(keyboard.KeyCode.from_char(p))
    return keys


def _build_hold_listener(combo: str, args: argparse.Namespace) -> keyboard.Listener:
    """Hold-to-record: recording starts when all keys are down, stops on any release."""
    required = _parse_combo(combo)
    currently_pressed: set = set()
    active = threading.Event()

    def on_press(key):
        currently_pressed.add(key)
        if required.issubset(currently_pressed) and not active.is_set():
            active.set()
            start_recording()

    def on_release(key):
        if key in currently_pressed:
            currently_pressed.discard(key)
        if active.is_set() and not required.issubset(currently_pressed):
            active.clear()
            text = stop_and_transcribe(args)
            if text:
                deliver_text(text, args)

    return keyboard.Listener(on_press=on_press, on_release=on_release)


def _build_toggle_listener(combo: str, args: argparse.Namespace) -> keyboard.GlobalHotKeys:
    """Toggle: first press starts recording, second press stops + transcribes."""
    toggled = threading.Event()

    def on_activate():
        if not toggled.is_set():
            toggled.set()
            start_recording()
        else:
            toggled.clear()
            text = stop_and_transcribe(args)
            if text:
                deliver_text(text, args)

    return keyboard.GlobalHotKeys({combo: on_activate})


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="Hold a hotkey → speak → release → text appears.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    p.add_argument(
        "--model",
        default="base",
        choices=["tiny", "base", "small", "medium", "large"],
        help="Whisper model size. Larger = more accurate but slower.",
    )
    p.add_argument(
        "--hotkey",
        default="<ctrl>+<alt>+r",
        help=(
            "Key combo to trigger recording. "
            "Use pynput syntax: <ctrl>+<alt>+r, <cmd>+r, etc."
        ),
    )
    p.add_argument(
        "--trigger",
        default="hold",
        choices=["hold", "toggle"],
        help="'hold' = record while keys held; 'toggle' = press once to start, again to stop.",
    )
    p.add_argument(
        "--type-direct",
        action="store_true",
        help="Type text into the focused window instead of copying to clipboard (requires xdotool).",
    )
    p.add_argument(
        "--send-enter",
        action="store_true",
        help="Press Enter after delivering the text.",
    )
    p.add_argument(
        "--language",
        default=None,
        help="Force Whisper to decode in this language (e.g. 'en'). Auto-detect if omitted.",
    )
    p.add_argument(
        "--device",
        default=None,
        type=int,
        help="sounddevice input device index. Run 'python -m sounddevice' to list devices.",
    )
    return p.parse_args()


def main() -> None:
    global model

    args = parse_args()

    print(f"🔄  Loading Whisper '{args.model}' model…", flush=True)
    model = whisper.load_model(args.model)
    print("✅  Model loaded.", flush=True)

    trigger_label = "Hold" if args.trigger == "hold" else "Toggle with"
    print(f"👂  {trigger_label} [{args.hotkey}] to record. Ctrl-C to quit.\n", flush=True)

    listener = build_hotkey_listener(args)

    with sd.InputStream(
        samplerate=SAMPLE_RATE,
        channels=1,
        dtype="float32",
        callback=audio_callback,
        device=args.device,
    ):
        with listener:
            try:
                stop_event.wait()  # block until Ctrl-C
            except KeyboardInterrupt:
                pass

    print("\n👋  Bye.", flush=True)


# Make Ctrl-C clean when blocking on stop_event
signal.signal(signal.SIGINT, lambda *_: stop_event.set())
signal.signal(signal.SIGTERM, lambda *_: stop_event.set())

if __name__ == "__main__":
    main()
