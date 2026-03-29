// whisper-stt: Hold a hotkey to record, release to transcribe.
//
// Transcription is delegated to an external whisper binary (default: the
// openai-whisper Python CLI, or whisper.cpp's whisper-cli).  Use --whisper-bin
// to point at a custom binary.
//
// Requirements:
//   - PortAudio development libraries (libportaudio-dev)
//   - A whisper binary on PATH (or --whisper-bin pointing to one)
//   - xdotool (optional, for --type-direct)
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/atotto/clipboard"
	"github.com/gordonklaus/portaudio"
	hook "github.com/robotn/gohook"
)

const (
	sampleRate      = 16_000 // Whisper expects 16 kHz
	framesPerBuffer = 1024
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type config struct {
	model      string
	whisperBin string
	hotkey     string
	trigger    string
	typeDirect bool
	sendEnter  bool
	language   string
	device     int
}

func parseFlags() *config {
	cfg := &config{}
	flag.StringVar(&cfg.model, "model", "base",
		"Whisper model size: tiny, base, small, medium, large")
	flag.StringVar(&cfg.whisperBin, "whisper-bin", "whisper",
		"Path to whisper binary (openai-whisper CLI or whisper.cpp whisper-cli)")
	flag.StringVar(&cfg.hotkey, "hotkey", "<ctrl>+<alt>+r",
		"Key combo to trigger recording (pynput syntax: <ctrl>+<alt>+r)")
	flag.StringVar(&cfg.trigger, "trigger", "hold",
		"Trigger mode: 'hold' = record while keys held; 'toggle' = press once to start, again to stop")
	flag.BoolVar(&cfg.typeDirect, "type-direct", false,
		"Type text into focused window instead of copying to clipboard (requires xdotool)")
	flag.BoolVar(&cfg.sendEnter, "send-enter", false,
		"Press Enter after delivering the text")
	flag.StringVar(&cfg.language, "language", "",
		"Force Whisper to decode in this language (e.g. 'en'). Auto-detect if empty.")
	flag.IntVar(&cfg.device, "device", -1,
		"PortAudio input device index. Run with --list-devices to list. -1 = default.")
	listDevices := flag.Bool("list-devices", false,
		"List available audio input devices and exit")
	flag.Parse()

	if *listDevices {
		mustListDevices()
		os.Exit(0)
	}
	return cfg
}

func mustListDevices() {
	if err := portaudio.Initialize(); err != nil {
		fmt.Fprintf(os.Stderr, "PortAudio init: %v\n", err)
		os.Exit(1)
	}
	defer portaudio.Terminate()
	devices, err := portaudio.Devices()
	if err != nil {
		fmt.Fprintf(os.Stderr, "List devices: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Available input devices:")
	for i, d := range devices {
		if d.MaxInputChannels > 0 {
			fmt.Printf("  [%d] %s\n", i, d.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// Recorder
// ---------------------------------------------------------------------------

type recorder struct {
	mu     sync.Mutex
	frames []float32
	active bool
}

func (r *recorder) isActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

func (r *recorder) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = nil
	r.active = true
	fmt.Println("🎙  Recording…")
}

// append copies incoming samples into the buffer while recording is active.
func (r *recorder) append(in []float32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		return
	}
	buf := make([]float32, len(in))
	copy(buf, in)
	r.frames = append(r.frames, buf...)
}

// stopAndGet atomically stops recording and returns the captured samples.
// Returns (nil, false) if recording was not active, guarding against duplicate calls.
func (r *recorder) stopAndGet() ([]float32, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		return nil, false
	}
	r.active = false
	result := make([]float32, len(r.frames))
	copy(result, r.frames)
	fmt.Println("⏹  Stopped. Transcribing…")
	return result, true
}

// ---------------------------------------------------------------------------
// WAV writing
// ---------------------------------------------------------------------------

// writeWAV writes float32 mono samples as a 16-bit PCM WAV file.
func writeWAV(f *os.File, samples []float32, rate int) error {
	numSamples := len(samples)
	dataSize := uint32(numSamples * 2) // 16-bit = 2 bytes/sample

	write16 := func(v uint16) { binary.Write(f, binary.LittleEndian, v) } //nolint:errcheck
	write32 := func(v uint32) { binary.Write(f, binary.LittleEndian, v) } //nolint:errcheck

	f.WriteString("RIFF")       //nolint:errcheck
	write32(36 + dataSize)      // chunk size
	f.WriteString("WAVEfmt ")   //nolint:errcheck
	write32(16)                 // subchunk1 size (PCM)
	write16(1)                  // audio format: PCM
	write16(1)                  // channels: mono
	write32(uint32(rate))       // sample rate
	write32(uint32(rate * 2))   // byte rate (rate * channels * bits/8)
	write16(2)                  // block align
	write16(16)                 // bits per sample
	f.WriteString("data")       //nolint:errcheck
	write32(dataSize)

	for _, s := range samples {
		clamped := math.Max(-1.0, math.Min(1.0, float64(s)))
		v := int16(clamped * 32767)
		if err := binary.Write(f, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Transcription — delegates to an external whisper binary
// ---------------------------------------------------------------------------

// timestampRe strips leading "[HH:MM:SS.mmm --> HH:MM:SS.mmm]" from whisper output lines.
var timestampRe = regexp.MustCompile(`^\[[0-9:.,\s\->]+\]\s*`)

// transcribe saves samples to a temp WAV, invokes the whisper binary,
// and returns the transcribed text.
func transcribe(samples []float32, cfg *config) (string, error) {
	// Write temp WAV file
	tmp, err := os.CreateTemp("", "whisper-stt-*.wav")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := writeWAV(tmp, samples, sampleRate); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write WAV: %w", err)
	}
	tmp.Close()

	// Build whisper command
	// openai-whisper: whisper FILE --model base --language en --output_format txt --output_dir DIR
	// whisper-cli:    whisper-cli -m model.bin -f FILE -l en
	// We target openai-whisper CLI; adjust --whisper-bin for whisper.cpp.
	outDir, err := os.MkdirTemp("", "whisper-stt-out-*")
	if err != nil {
		return "", fmt.Errorf("create output dir: %w", err)
	}
	defer os.RemoveAll(outDir)

	args := []string{
		tmpPath,
		"--model", cfg.model,
		"--output_format", "txt",
		"--output_dir", outDir,
	}
	if cfg.language != "" {
		args = append(args, "--language", cfg.language)
	}

	var stderr bytes.Buffer
	cmd := exec.Command(cfg.whisperBin, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("whisper exited: %w\nstderr: %s", err, stderr.String())
	}

	// openai-whisper creates <basename>.txt in outDir
	base := strings.TrimSuffix(filepath.Base(tmpPath), ".wav")
	txtPath := filepath.Join(outDir, base+".txt")
	data, err := os.ReadFile(txtPath)
	if err != nil {
		// Fall back to parsing stdout-style output (whisper.cpp may write directly)
		return parseTimestampedLines(stderr.String()), nil
	}
	return strings.TrimSpace(string(data)), nil
}

// parseTimestampedLines extracts text from whisper timestamp-prefixed output lines.
// Handles lines like: [00:00.000 --> 00:05.000]  Hello world.
func parseTimestampedLines(output string) string {
	var parts []string
	for _, line := range strings.Split(output, "\n") {
		line = timestampRe.ReplaceAllString(strings.TrimSpace(line), "")
		if line != "" {
			parts = append(parts, line)
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// ---------------------------------------------------------------------------
// Text delivery
// ---------------------------------------------------------------------------

func deliverText(text string, cfg *config) {
	if cfg.typeDirect {
		typeText(text, cfg)
		return
	}
	out := text
	if cfg.sendEnter {
		out += "\n"
	}
	if err := clipboard.WriteAll(out); err != nil {
		fmt.Fprintf(os.Stderr, "Clipboard error: %v\n", err)
		return
	}
	fmt.Println("📋  Copied to clipboard.")
}

func typeText(text string, cfg *config) {
	if _, err := exec.LookPath("xdotool"); err != nil {
		fmt.Fprintln(os.Stderr, "❌  xdotool not found. Install: sudo apt install xdotool")
		clipboard.WriteAll(text) //nolint:errcheck
		fmt.Println("📋  Fell back to clipboard.")
		return
	}
	exec.Command("xdotool", "type", "--clearmodifiers", "--", text).Run() //nolint:errcheck
	if cfg.sendEnter {
		exec.Command("xdotool", "key", "Return").Run() //nolint:errcheck
	}
	fmt.Println("⌨️  Typed into active window.")
}

// ---------------------------------------------------------------------------
// Hotkey helpers
// ---------------------------------------------------------------------------

// parseComboKeys converts a pynput-style combo string to the slice of key name
// strings understood by gohook, e.g. "<ctrl>+<alt>+r" → ["ctrl","alt","r"].
func parseComboKeys(combo string) []string {
	parts := strings.Split(combo, "+")
	keys := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "<>")
		p = strings.ToLower(p)
		// Normalise common aliases to the names gohook recognises.
		switch p {
		case "cmd", "win":
			p = "super"
		case "control", "ctrl_l", "control_l":
			p = "ctrl"
		case "alt_l":
			p = "alt"
		case "shift_l":
			p = "shift"
		}
		keys = append(keys, p)
	}
	return keys
}

// ---------------------------------------------------------------------------
// Hotkey listeners
// ---------------------------------------------------------------------------

// handleSamples transcribes captured audio and delivers the resulting text.
func handleSamples(samples []float32, cfg *config) {
	if len(samples) == 0 {
		fmt.Fprintln(os.Stderr, "⚠️  No audio captured.")
		return
	}
	text, err := transcribe(samples, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Transcription error: %v\n", err)
		return
	}
	if text == "" {
		fmt.Fprintln(os.Stderr, "⚠️  Nothing recognised.")
		return
	}
	fmt.Printf("📝  %s\n", text)
	deliverText(text, cfg)
}

// runHoldListener starts recording when all combo keys are held and stops
// (triggering transcription) when any of them is released.
func runHoldListener(rec *recorder, cfg *config, keys []string) {
	// Start recording on full combo press.
	hook.Register(hook.KeyDown, keys, func(e hook.Event) {
		if !rec.isActive() {
			rec.start()
		}
	})

	// Stop on release of any individual key in the combo.
	// stopAndGet is atomic so only the first goroutine that wins does work.
	for _, k := range keys {
		k := k // capture loop variable
		hook.Register(hook.KeyUp, []string{k}, func(e hook.Event) {
			go func() {
				samples, ok := rec.stopAndGet()
				if !ok {
					return
				}
				handleSamples(samples, cfg)
			}()
		})
	}

	s := hook.Start()
	defer hook.End()
	<-hook.Process(s)
}

// runToggleListener toggles recording on each successive press of the combo.
func runToggleListener(rec *recorder, cfg *config, keys []string) {
	var mu sync.Mutex
	toggled := false

	hook.Register(hook.KeyDown, keys, func(e hook.Event) {
		mu.Lock()
		defer mu.Unlock()
		if !toggled {
			toggled = true
			rec.start()
		} else {
			toggled = false
			go func() {
				samples, ok := rec.stopAndGet()
				if !ok {
					return
				}
				handleSamples(samples, cfg)
			}()
		}
	})

	s := hook.Start()
	defer hook.End()
	<-hook.Process(s)
}

// ---------------------------------------------------------------------------
// Audio stream
// ---------------------------------------------------------------------------

func openAudioStream(rec *recorder, cfg *config) (*portaudio.Stream, error) {
	callback := func(in []float32) { rec.append(in) }

	if cfg.device >= 0 {
		devices, err := portaudio.Devices()
		if err != nil {
			return nil, fmt.Errorf("list devices: %w", err)
		}
		if cfg.device >= len(devices) {
			return nil, fmt.Errorf("device index %d out of range (have %d devices)",
				cfg.device, len(devices))
		}
		dev := devices[cfg.device]
		params := portaudio.StreamParameters{
			Input: portaudio.StreamDeviceParameters{
				Device:   dev,
				Channels: 1,
				Latency:  dev.DefaultLowInputLatency,
			},
			SampleRate:      float64(sampleRate),
			FramesPerBuffer: framesPerBuffer,
		}
		return portaudio.OpenStream(params, callback)
	}

	return portaudio.OpenDefaultStream(1, 0, float64(sampleRate), framesPerBuffer, callback)
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() {
	cfg := parseFlags()

	// Validate whisper binary is reachable.
	if _, err := exec.LookPath(cfg.whisperBin); err != nil {
		fmt.Fprintf(os.Stderr,
			"Whisper binary %q not found on PATH. Install openai-whisper or set --whisper-bin.\n",
			cfg.whisperBin)
		os.Exit(1)
	}
	fmt.Printf("🔄  Using Whisper binary %q with model %q…\n", cfg.whisperBin, cfg.model)

	// Initialise PortAudio.
	if err := portaudio.Initialize(); err != nil {
		fmt.Fprintf(os.Stderr, "PortAudio init: %v\n", err)
		os.Exit(1)
	}
	defer portaudio.Terminate()

	rec := &recorder{}

	stream, err := openAudioStream(rec, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open audio stream: %v\n", err)
		os.Exit(1)
	}
	if err := stream.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Start audio stream: %v\n", err)
		os.Exit(1)
	}
	defer stream.Stop()  //nolint:errcheck
	defer stream.Close() //nolint:errcheck

	keys := parseComboKeys(cfg.hotkey)
	triggerLabel := "Hold"
	if cfg.trigger == "toggle" {
		triggerLabel = "Toggle with"
	}
	fmt.Printf("👂  %s [%s] to record. Ctrl-C to quit.\n\n", triggerLabel, cfg.hotkey)

	go func() {
		if cfg.trigger == "toggle" {
			runToggleListener(rec, cfg, keys)
		} else {
			runHoldListener(rec, cfg, keys)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	fmt.Println("\n👋  Bye.")
}
