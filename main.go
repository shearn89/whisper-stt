// whisper-stt: Hold a hotkey to record, release to transcribe.
//
// Configuration is loaded from ~/.config/whisper-stt/config.toml by default.
// All settings can be overridden with CLI flags.
//
// Requirements:
//   - PortAudio development libraries (libportaudio-dev / portaudio19-dev)
//   - A whisper binary on PATH (or whisper_bin in config / --whisper-bin flag)
//   - xdotool (optional, for type_direct)
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

	"github.com/BurntSushi/toml"
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

// config holds all runtime settings.  Fields are exported so the TOML decoder
// can populate them; toml tags match the snake_case keys in config.toml.
type config struct {
	Model      string `toml:"model"`
	WhisperBin string `toml:"whisper_bin"`
	Hotkey     string `toml:"hotkey"`
	Trigger    string `toml:"trigger"`
	TypeDirect bool   `toml:"type_direct"`
	SendEnter  bool   `toml:"send_enter"`
	Language   string `toml:"language"`
	Device     int    `toml:"device"`
}

func defaultConfig() *config {
	return &config{
		Model:      "base",
		WhisperBin: "whisper",
		Hotkey:     "<ctrl>+<alt>+r",
		Trigger:    "hold",
		Device:     -1,
	}
}

func defaultConfigPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(base, "whisper-stt", "config.toml")
}

// loadConfig builds a config by layering: defaults → config file → CLI flags.
func loadConfig() *config {
	cfg := defaultConfig()

	// Pre-scan argv to find --config before flag.Parse so we know which file
	// to load before registering flag defaults.
	configPath := defaultConfigPath()
	for i, arg := range os.Args[1:] {
		switch {
		case strings.HasPrefix(arg, "--config="):
			configPath = strings.TrimPrefix(arg, "--config=")
		case arg == "--config" && i+2 < len(os.Args):
			configPath = os.Args[i+2]
		}
	}

	// Load TOML config file; missing file is fine, any other error is fatal.
	if raw, err := os.ReadFile(configPath); err == nil {
		if _, err := toml.Decode(string(raw), cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Config parse error (%s): %v\n", configPath, err)
			os.Exit(1)
		}
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Config read error (%s): %v\n", configPath, err)
		os.Exit(1)
	}

	// Register CLI flags using loaded config values as their defaults so that
	// an explicit flag always wins over the config file.
	flag.StringVar(&cfg.Model, "model", cfg.Model,
		"Whisper model size: tiny, base, small, medium, large")
	flag.StringVar(&cfg.WhisperBin, "whisper-bin", cfg.WhisperBin,
		"Path to whisper binary (openai-whisper CLI or whisper.cpp whisper-cli)")
	flag.StringVar(&cfg.Hotkey, "hotkey", cfg.Hotkey,
		"Key combo to trigger recording (pynput syntax: <ctrl>+<alt>+r)")
	flag.StringVar(&cfg.Trigger, "trigger", cfg.Trigger,
		"Trigger mode: 'hold' = record while keys held; 'toggle' = press once to start, again to stop")
	flag.BoolVar(&cfg.TypeDirect, "type-direct", cfg.TypeDirect,
		"Type text into focused window instead of copying to clipboard (requires xdotool)")
	flag.BoolVar(&cfg.SendEnter, "send-enter", cfg.SendEnter,
		"Press Enter after delivering the text")
	flag.StringVar(&cfg.Language, "language", cfg.Language,
		"Force Whisper to decode in this language (e.g. 'en'). Auto-detect if empty.")
	flag.IntVar(&cfg.Device, "device", cfg.Device,
		"PortAudio input device index. Run --list-devices to list. -1 = default.")

	flag.String("config", configPath,
		"Path to TOML config file (default: ~/.config/whisper-stt/config.toml)")
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
	dataSize := uint32(len(samples) * 2) // 16-bit = 2 bytes/sample

	write16 := func(v uint16) { binary.Write(f, binary.LittleEndian, v) } //nolint:errcheck
	write32 := func(v uint32) { binary.Write(f, binary.LittleEndian, v) } //nolint:errcheck

	f.WriteString("RIFF")      //nolint:errcheck
	write32(36 + dataSize)     // chunk size
	f.WriteString("WAVEfmt ")  //nolint:errcheck
	write32(16)                // subchunk1 size (PCM)
	write16(1)                 // audio format: PCM
	write16(1)                 // channels: mono
	write32(uint32(rate))      // sample rate
	write32(uint32(rate * 2))  // byte rate
	write16(2)                 // block align
	write16(16)                // bits per sample
	f.WriteString("data")      //nolint:errcheck
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

// timestampRe strips leading "[HH:MM:SS.mmm --> HH:MM:SS.mmm]" from output lines.
var timestampRe = regexp.MustCompile(`^\[[0-9:.,\s\->]+\]\s*`)

// transcribe saves samples to a temp WAV, invokes the whisper binary,
// and returns the transcribed text.
func transcribe(samples []float32, cfg *config) (string, error) {
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

	outDir, err := os.MkdirTemp("", "whisper-stt-out-*")
	if err != nil {
		return "", fmt.Errorf("create output dir: %w", err)
	}
	defer os.RemoveAll(outDir)

	args := []string{
		tmpPath,
		"--model", cfg.Model,
		"--output_format", "txt",
		"--output_dir", outDir,
	}
	if cfg.Language != "" {
		args = append(args, "--language", cfg.Language)
	}

	var stderr bytes.Buffer
	cmd := exec.Command(cfg.WhisperBin, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("whisper exited: %w\nstderr: %s", err, stderr.String())
	}

	// openai-whisper creates <basename>.txt in outDir
	base := strings.TrimSuffix(filepath.Base(tmpPath), ".wav")
	data, err := os.ReadFile(filepath.Join(outDir, base+".txt"))
	if err != nil {
		// Fallback: parse timestamp-prefixed lines from stderr (whisper.cpp style)
		return parseTimestampedLines(stderr.String()), nil
	}
	return strings.TrimSpace(string(data)), nil
}

// parseTimestampedLines extracts text from lines like:
// [00:00.000 --> 00:05.000]  Hello world.
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
	if cfg.TypeDirect {
		typeText(text, cfg)
		return
	}
	out := text
	if cfg.SendEnter {
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
	if cfg.SendEnter {
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
	hook.Register(hook.KeyDown, keys, func(e hook.Event) {
		if !rec.isActive() {
			rec.start()
		}
	})

	// stopAndGet is atomic so only the first goroutine that wins does work.
	for _, k := range keys {
		k := k
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

	if cfg.Device >= 0 {
		devices, err := portaudio.Devices()
		if err != nil {
			return nil, fmt.Errorf("list devices: %w", err)
		}
		if cfg.Device >= len(devices) {
			return nil, fmt.Errorf("device index %d out of range (have %d devices)",
				cfg.Device, len(devices))
		}
		dev := devices[cfg.Device]
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
	cfg := loadConfig()

	if _, err := exec.LookPath(cfg.WhisperBin); err != nil {
		fmt.Fprintf(os.Stderr,
			"Whisper binary %q not found on PATH. Install openai-whisper or set whisper_bin in config.\n",
			cfg.WhisperBin)
		os.Exit(1)
	}
	fmt.Printf("🔄  Using Whisper binary %q with model %q…\n", cfg.WhisperBin, cfg.Model)

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

	keys := parseComboKeys(cfg.Hotkey)
	triggerLabel := "Hold"
	if cfg.Trigger == "toggle" {
		triggerLabel = "Toggle with"
	}
	fmt.Printf("👂  %s [%s] to record. Ctrl-C to quit.\n\n", triggerLabel, cfg.Hotkey)

	go func() {
		if cfg.Trigger == "toggle" {
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
