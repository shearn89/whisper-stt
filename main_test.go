package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gordonklaus/portaudio"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// whisperBin returns the path to a test-local executable copy of
// testdata/fake-whisper.sh.  The copy lives in a t.TempDir() so it is cleaned
// up automatically.
func whisperBin(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("testdata/fake-whisper.sh")
	if err != nil {
		t.Fatalf("read fake-whisper.sh: %v", err)
	}
	path := filepath.Join(t.TempDir(), "whisper")
	if err := os.WriteFile(path, src, 0755); err != nil {
		t.Fatalf("write fake whisper: %v", err)
	}
	return path
}

// writeTOML writes content to a temp file and returns its path.
func writeTOML(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.toml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

// captureCmds replaces newCmd with a recorder that logs every (name, args...)
// call and runs "sh -c 'exit 0'" instead — i.e. a no-op that always succeeds.
// Returns the call log (by pointer, updated in place) and a restore func.
func captureCmds(t *testing.T) (*[][]string, func()) {
	t.Helper()
	var mu sync.Mutex
	calls := &[][]string{}
	orig := newCmd
	newCmd = func(name string, args ...string) *exec.Cmd {
		mu.Lock()
		*calls = append(*calls, append([]string{name}, args...))
		mu.Unlock()
		return exec.Command("sh", "-c", "exit 0")
	}
	return calls, func() { newCmd = orig }
}

// captureClipboard replaces clipboardWrite with a function that stores the
// last written value.  Returns a pointer to the stored string and a restore func.
func captureClipboard(t *testing.T) (*string, func()) {
	t.Helper()
	written := new(string)
	orig := clipboardWrite
	clipboardWrite = func(text string) error {
		*written = text
		return nil
	}
	return written, func() { clipboardWrite = orig }
}

// fakeStream is a no-op audioStream for tests.
type fakeStream struct{ started bool }

func (s *fakeStream) Start() error { s.started = true; return nil }
func (s *fakeStream) Stop() error  { return nil }
func (s *fakeStream) Close() error { return nil }

// silentSamples returns n float32 samples all set to zero.
func silentSamples(n int) []float32 { return make([]float32, n) }

// ---------------------------------------------------------------------------
// defaultConfigPath
// ---------------------------------------------------------------------------

func TestDefaultConfigPath_WithXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/cfg")
	got := defaultConfigPath()
	want := "/custom/cfg/whisper-stt/config.toml"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDefaultConfigPath_WithoutXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/testuser")
	got := defaultConfigPath()
	want := "/home/testuser/.config/whisper-stt/config.toml"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// loadConfig
// ---------------------------------------------------------------------------

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := loadConfig([]string{"--config", "/nonexistent/path.toml"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "base" {
		t.Errorf("Model: got %q, want %q", cfg.Model, "base")
	}
	if cfg.WhisperBin != "whisper" {
		t.Errorf("WhisperBin: got %q, want %q", cfg.WhisperBin, "whisper")
	}
	if cfg.Hotkey != "<ctrl>+<alt>+r" {
		t.Errorf("Hotkey: got %q, want %q", cfg.Hotkey, "<ctrl>+<alt>+r")
	}
	if cfg.Trigger != "hold" {
		t.Errorf("Trigger: got %q, want %q", cfg.Trigger, "hold")
	}
	if cfg.Device != -1 {
		t.Errorf("Device: got %d, want -1", cfg.Device)
	}
	if cfg.TypeDirect || cfg.SendEnter {
		t.Errorf("TypeDirect/SendEnter should default to false")
	}
}

func TestLoadConfig_FromFile(t *testing.T) {
	path := writeTOML(t, `
model       = "small"
whisper_bin = "/usr/local/bin/whisper"
hotkey      = "<alt>+r"
trigger     = "toggle"
type_direct = true
send_enter  = true
language    = "de"
device      = 2
`)
	cfg, err := loadConfig([]string{"--config", path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "small" {
		t.Errorf("Model: got %q", cfg.Model)
	}
	if cfg.WhisperBin != "/usr/local/bin/whisper" {
		t.Errorf("WhisperBin: got %q", cfg.WhisperBin)
	}
	if cfg.Hotkey != "<alt>+r" {
		t.Errorf("Hotkey: got %q", cfg.Hotkey)
	}
	if cfg.Trigger != "toggle" {
		t.Errorf("Trigger: got %q", cfg.Trigger)
	}
	if !cfg.TypeDirect {
		t.Error("TypeDirect should be true")
	}
	if !cfg.SendEnter {
		t.Error("SendEnter should be true")
	}
	if cfg.Language != "de" {
		t.Errorf("Language: got %q", cfg.Language)
	}
	if cfg.Device != 2 {
		t.Errorf("Device: got %d", cfg.Device)
	}
}

func TestLoadConfig_CLIOverridesFile(t *testing.T) {
	path := writeTOML(t, `model = "large"`)
	cfg, err := loadConfig([]string{"--config", path, "--model", "tiny"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "tiny" {
		t.Errorf("CLI flag should override file: got %q, want %q", cfg.Model, "tiny")
	}
}

func TestLoadConfig_MissingFileIsOK(t *testing.T) {
	_, err := loadConfig([]string{"--config", "/does/not/exist.toml"})
	if err != nil {
		t.Errorf("missing config file should not be an error, got: %v", err)
	}
}

func TestLoadConfig_InvalidTOML(t *testing.T) {
	path := writeTOML(t, `not valid toml ][`)
	_, err := loadConfig([]string{"--config", path})
	if err == nil {
		t.Error("expected error for invalid TOML, got nil")
	}
	if !strings.Contains(err.Error(), "config parse error") {
		t.Errorf("error message should mention 'config parse error', got: %v", err)
	}
}

func TestLoadConfig_ConfigFlagChangesFile(t *testing.T) {
	// First file has model=large, second has model=tiny.
	_ = writeTOML(t, `model = "large"`)
	path2 := writeTOML(t, `model = "tiny"`)

	cfg, err := loadConfig([]string{"--config=" + path2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "tiny" {
		t.Errorf("--config= should select second file: got %q", cfg.Model)
	}
}

func TestLoadConfig_UnknownFlagReturnsError(t *testing.T) {
	_, err := loadConfig([]string{"--no-such-flag"})
	if err == nil {
		t.Error("expected error for unknown flag, got nil")
	}
}

// ---------------------------------------------------------------------------
// recorder
// ---------------------------------------------------------------------------

func TestRecorder_InitialState(t *testing.T) {
	r := &recorder{}
	if r.isActive() {
		t.Error("new recorder should not be active")
	}
}

func TestRecorder_StartActivates(t *testing.T) {
	r := &recorder{}
	r.frames = []float32{1, 2, 3} // pre-populate to verify it is cleared
	r.start()
	if !r.isActive() {
		t.Error("recorder should be active after start()")
	}
	if len(r.frames) != 0 {
		t.Errorf("start() should clear frames, got len=%d", len(r.frames))
	}
}

func TestRecorder_AppendWhileActive(t *testing.T) {
	r := &recorder{}
	r.start()
	r.append([]float32{0.1, 0.2})
	r.append([]float32{0.3})
	samples, ok := r.stopAndGet()
	if !ok {
		t.Fatal("stopAndGet returned false on active recorder")
	}
	if len(samples) != 3 {
		t.Errorf("expected 3 samples, got %d", len(samples))
	}
}

func TestRecorder_AppendWhileInactiveIsNoop(t *testing.T) {
	r := &recorder{}
	// not started — append should be a no-op
	r.append([]float32{0.5, 0.6})
	if len(r.frames) != 0 {
		t.Errorf("append while inactive should not store samples, got %d frames", len(r.frames))
	}
}

func TestRecorder_AppendIsolatesBuffer(t *testing.T) {
	r := &recorder{}
	r.start()
	original := []float32{0.1, 0.2, 0.3}
	r.append(original)
	// Mutate the original slice; the recorder's copy should be unaffected.
	original[0] = 999
	samples, _ := r.stopAndGet()
	if samples[0] == 999 {
		t.Error("recorder should copy samples, not hold a reference to the caller's slice")
	}
}

func TestRecorder_StopAndGet_ReturnsFalseWhenInactive(t *testing.T) {
	r := &recorder{}
	_, ok := r.stopAndGet()
	if ok {
		t.Error("stopAndGet on inactive recorder should return false")
	}
}

func TestRecorder_StopAndGet_IdempotentOnDoubleStop(t *testing.T) {
	r := &recorder{}
	r.start()
	r.append(silentSamples(100))

	samples1, ok1 := r.stopAndGet()
	samples2, ok2 := r.stopAndGet()

	if !ok1 {
		t.Error("first stop should return true")
	}
	if ok2 {
		t.Error("second stop should return false")
	}
	if len(samples1) != 100 {
		t.Errorf("first stop should return all samples, got %d", len(samples1))
	}
	if samples2 != nil {
		t.Error("second stop should return nil samples")
	}
}

func TestRecorder_Concurrent(t *testing.T) {
	// Run with -race to check for data races.
	r := &recorder{}
	r.start()

	var wg sync.WaitGroup
	// 10 goroutines appending concurrently
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.append(silentSamples(64))
		}()
	}
	// One goroutine stopping
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.stopAndGet()
	}()
	wg.Wait()
}

// ---------------------------------------------------------------------------
// writeWAV
// ---------------------------------------------------------------------------

func TestWriteWAV_Header(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "test-*.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	samples := silentSamples(4)
	if err := writeWAV(f, samples, 16000); err != nil {
		t.Fatalf("writeWAV: %v", err)
	}

	f.Seek(0, 0)
	buf, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}

	// RIFF chunk descriptor
	if string(buf[0:4]) != "RIFF" {
		t.Errorf("chunk ID: got %q, want %q", buf[0:4], "RIFF")
	}
	if string(buf[8:12]) != "WAVE" {
		t.Errorf("format: got %q, want %q", buf[8:12], "WAVE")
	}
	// fmt sub-chunk
	if string(buf[12:16]) != "fmt " {
		t.Errorf("sub-chunk1 ID: got %q, want %q", buf[12:16], "fmt ")
	}
	audioFmt := binary.LittleEndian.Uint16(buf[20:22])
	if audioFmt != 1 {
		t.Errorf("audio format: got %d, want 1 (PCM)", audioFmt)
	}
	channels := binary.LittleEndian.Uint16(buf[22:24])
	if channels != 1 {
		t.Errorf("channels: got %d, want 1", channels)
	}
	sampleRateField := binary.LittleEndian.Uint32(buf[24:28])
	if sampleRateField != 16000 {
		t.Errorf("sample rate: got %d, want 16000", sampleRateField)
	}
	bitsPerSample := binary.LittleEndian.Uint16(buf[34:36])
	if bitsPerSample != 16 {
		t.Errorf("bits per sample: got %d, want 16", bitsPerSample)
	}
	// data sub-chunk
	if string(buf[36:40]) != "data" {
		t.Errorf("sub-chunk2 ID: got %q, want %q", buf[36:40], "data")
	}
	dataSize := binary.LittleEndian.Uint32(buf[40:44])
	if dataSize != uint32(len(samples)*2) {
		t.Errorf("data size: got %d, want %d", dataSize, len(samples)*2)
	}
}

func TestWriteWAV_SampleConversion(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "test-*.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// 1.0 → 32767, -1.0 → -32767, 0.5 → 16383
	samples := []float32{1.0, -1.0, 0.5, 0.0}
	if err := writeWAV(f, samples, 16000); err != nil {
		t.Fatalf("writeWAV: %v", err)
	}

	f.Seek(0, 0)
	buf, _ := os.ReadFile(f.Name())
	dataOffset := 44 // standard WAV header is 44 bytes

	tests := []struct {
		name    string
		index   int
		wantMin int16
		wantMax int16
	}{
		{"full positive", 0, 32766, 32767},
		{"full negative", 1, -32767, -32766},
		{"half positive", 2, 16382, 16384},
		{"zero", 3, -1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			offset := dataOffset + tt.index*2
			got := int16(binary.LittleEndian.Uint16(buf[offset : offset+2]))
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("sample[%d] = %d, want in [%d, %d]", tt.index, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestWriteWAV_Clamping(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "test-*.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Values well outside [-1, 1] must be clamped, not overflow.
	samples := []float32{100.0, -200.0}
	if err := writeWAV(f, samples, 16000); err != nil {
		t.Fatalf("writeWAV: %v", err)
	}

	f.Seek(0, 0)
	buf, _ := os.ReadFile(f.Name())

	v0 := int16(binary.LittleEndian.Uint16(buf[44:46]))
	v1 := int16(binary.LittleEndian.Uint16(buf[46:48]))
	if v0 != int16(math.Round(1.0*32767)) {
		t.Errorf("positive overflow not clamped: got %d", v0)
	}
	if v1 != int16(math.Round(-1.0*32767)) {
		t.Errorf("negative overflow not clamped: got %d", v1)
	}
}

// ---------------------------------------------------------------------------
// parseTimestampedLines
// ---------------------------------------------------------------------------

func TestParseTimestampedLines(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "single line",
			input: "[00:00.000 --> 00:03.000]  Hello world.",
			want:  "Hello world.",
		},
		{
			name:  "multiple lines joined",
			input: "[00:00.000 --> 00:02.000]  First.\n[00:02.000 --> 00:04.000]  Second.",
			want:  "First. Second.",
		},
		{
			name:  "non-timestamp lines ignored",
			input: "Detecting language...\n[00:00.000 --> 00:02.000]  Hello.",
			want:  "Detecting language... Hello.",
		},
		{
			name:  "empty input",
			input: "",
			want:  "",
		},
		{
			name:  "whitespace-only lines skipped",
			input: "[00:00.000 --> 00:01.000]  \n[00:01.000 --> 00:02.000]  Text.",
			want:  "Text.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTimestampedLines(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseComboKeys
// ---------------------------------------------------------------------------

func TestParseComboKeys(t *testing.T) {
	tests := []struct {
		combo string
		want  []string
	}{
		{"<ctrl>+<alt>+r", []string{"ctrl", "alt", "r"}},
		{"<shift>+a", []string{"shift", "a"}},
		{"<alt_r>", []string{"alt_r"}},
		{"<ctrl_l>+r", []string{"ctrl", "r"}},   // ctrl_l → ctrl
		{"<control>+x", []string{"ctrl", "x"}},  // control → ctrl
		{"<alt_l>+z", []string{"alt", "z"}},     // alt_l → alt
		{"<shift_l>+s", []string{"shift", "s"}}, // shift_l → shift
		{"<cmd>+space", []string{"super", "space"}},
		{"<win>+r", []string{"super", "r"}},
		{"r", []string{"r"}},
	}
	for _, tt := range tests {
		t.Run(tt.combo, func(t *testing.T) {
			got := parseComboKeys(tt.combo)
			if len(got) != len(tt.want) {
				t.Fatalf("len: got %d (%v), want %d (%v)", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// transcribe  (uses the fake whisper binary as the transport layer)
// ---------------------------------------------------------------------------

func TestTranscribe_Success(t *testing.T) {
	bin := whisperBin(t)
	t.Setenv("WHISPER_TRANSCRIPT", "Hello from whisper.")

	cfg := &config{Model: "base", WhisperBin: bin}
	text, err := transcribe(silentSamples(sampleRate), cfg)
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if text != "Hello from whisper." {
		t.Errorf("got %q, want %q", text, "Hello from whisper.")
	}
}

func TestTranscribe_WithLanguage(t *testing.T) {
	// The fake whisper script doesn't use --language, but we verify it doesn't
	// cause an error and the transcript is still returned correctly.
	bin := whisperBin(t)
	t.Setenv("WHISPER_TRANSCRIPT", "Bonjour.")

	cfg := &config{Model: "base", WhisperBin: bin, Language: "fr"}
	text, err := transcribe(silentSamples(sampleRate), cfg)
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if text != "Bonjour." {
		t.Errorf("got %q, want %q", text, "Bonjour.")
	}
}

func TestTranscribe_WhisperExitError(t *testing.T) {
	bin := whisperBin(t)
	t.Setenv("WHISPER_EXIT_CODE", "1")

	cfg := &config{Model: "base", WhisperBin: bin}
	_, err := transcribe(silentSamples(sampleRate), cfg)
	if err == nil {
		t.Error("expected error when whisper exits non-zero, got nil")
	}
	if !strings.Contains(err.Error(), "whisper exited") {
		t.Errorf("error should mention 'whisper exited', got: %v", err)
	}
}

func TestTranscribe_FallbackTimestampParsing(t *testing.T) {
	// When the whisper binary writes to stderr but creates no .txt file,
	// transcribe should fall back to parseTimestampedLines.
	bin := whisperBin(t)
	t.Setenv("WHISPER_STDERR_TEXT", "[00:00.000 --> 00:03.000]  Fallback text.")

	cfg := &config{Model: "base", WhisperBin: bin}
	text, err := transcribe(silentSamples(sampleRate), cfg)
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if text != "Fallback text." {
		t.Errorf("got %q, want %q", text, "Fallback text.")
	}
}

func TestTranscribe_EmptyTranscript(t *testing.T) {
	bin := whisperBin(t)
	t.Setenv("WHISPER_TRANSCRIPT", "   ") // whitespace only → trimmed to ""

	cfg := &config{Model: "base", WhisperBin: bin}
	text, err := transcribe(silentSamples(sampleRate), cfg)
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty string, got %q", text)
	}
}

func TestTranscribe_MissingBinary(t *testing.T) {
	cfg := &config{Model: "base", WhisperBin: "/nonexistent/whisper-binary"}
	_, err := transcribe(silentSamples(sampleRate), cfg)
	if err == nil {
		t.Error("expected error for missing binary, got nil")
	}
}

// ---------------------------------------------------------------------------
// deliverText / typeText  (transport layer: clipboard + xdotool)
// ---------------------------------------------------------------------------

func TestDeliverText_Clipboard(t *testing.T) {
	written, restore := captureClipboard(t)
	defer restore()

	cfg := &config{}
	deliverText("hello clipboard", cfg)

	if *written != "hello clipboard" {
		t.Errorf("clipboard: got %q, want %q", *written, "hello clipboard")
	}
}

func TestDeliverText_Clipboard_WithSendEnter(t *testing.T) {
	written, restore := captureClipboard(t)
	defer restore()

	cfg := &config{SendEnter: true}
	deliverText("hello", cfg)

	if *written != "hello\n" {
		t.Errorf("clipboard with SendEnter: got %q, want %q", *written, "hello\n")
	}
}

func TestDeliverText_Clipboard_WriteError(t *testing.T) {
	orig := clipboardWrite
	defer func() { clipboardWrite = orig }()
	clipboardWrite = func(string) error { return errors.New("clipboard unavailable") }

	// Should not panic — just print to stderr.
	cfg := &config{}
	deliverText("text", cfg)
}

func TestDeliverText_TypeDirect(t *testing.T) {
	calls, restore := captureCmds(t)
	defer restore()

	// Make lookPath believe xdotool is present.
	origLP := lookPath
	defer func() { lookPath = origLP }()
	lookPath = func(name string) (string, error) {
		if name == "xdotool" {
			return "/usr/bin/xdotool", nil
		}
		return exec.LookPath(name)
	}

	cfg := &config{TypeDirect: true}
	deliverText("typed text", cfg)

	// Expect exactly one "xdotool type" call.
	found := false
	for _, c := range *calls {
		if len(c) >= 2 && c[0] == "xdotool" && c[1] == "type" {
			found = true
			// Last arg should be the text
			if c[len(c)-1] != "typed text" {
				t.Errorf("xdotool type last arg: got %q, want %q", c[len(c)-1], "typed text")
			}
		}
	}
	if !found {
		t.Errorf("expected xdotool type call, got: %v", *calls)
	}
}

func TestDeliverText_TypeDirect_WithSendEnter(t *testing.T) {
	calls, restore := captureCmds(t)
	defer restore()

	origLP := lookPath
	defer func() { lookPath = origLP }()
	lookPath = func(name string) (string, error) {
		if name == "xdotool" {
			return "/usr/bin/xdotool", nil
		}
		return exec.LookPath(name)
	}

	cfg := &config{TypeDirect: true, SendEnter: true}
	deliverText("send it", cfg)

	// Expect both "xdotool type" and "xdotool key Return".
	var hasType, hasReturn bool
	for _, c := range *calls {
		if len(c) >= 2 && c[0] == "xdotool" && c[1] == "type" {
			hasType = true
		}
		if len(c) >= 3 && c[0] == "xdotool" && c[1] == "key" && c[2] == "Return" {
			hasReturn = true
		}
	}
	if !hasType {
		t.Error("expected xdotool type call")
	}
	if !hasReturn {
		t.Errorf("expected xdotool key Return call, got: %v", *calls)
	}
}

func TestDeliverText_TypeDirect_XdotoolNotFound(t *testing.T) {
	written, restoreClip := captureClipboard(t)
	defer restoreClip()

	origLP := lookPath
	defer func() { lookPath = origLP }()
	lookPath = func(string) (string, error) { return "", errors.New("not found") }

	cfg := &config{TypeDirect: true}
	deliverText("fallback text", cfg)

	// Should fall back to clipboard.
	if *written != "fallback text" {
		t.Errorf("fallback clipboard: got %q, want %q", *written, "fallback text")
	}
}

// ---------------------------------------------------------------------------
// handleSamples  (end-to-end: transcribe → deliver, mocking the transport)
// ---------------------------------------------------------------------------

func TestHandleSamples_Empty(t *testing.T) {
	// No samples → should return without calling transcribe at all.
	cfg := &config{WhisperBin: "/nonexistent/should-not-be-called"}
	// If transcribe were called with this binary it would error; the fact that
	// handleSamples returns cleanly proves it short-circuits on empty input.
	handleSamples(nil, cfg)
	handleSamples([]float32{}, cfg)
}

func TestHandleSamples_Success(t *testing.T) {
	bin := whisperBin(t)
	t.Setenv("WHISPER_TRANSCRIPT", "Integration test text.")

	written, restore := captureClipboard(t)
	defer restore()

	cfg := &config{Model: "base", WhisperBin: bin}
	handleSamples(silentSamples(sampleRate), cfg)

	if *written != "Integration test text." {
		t.Errorf("clipboard: got %q, want %q", *written, "Integration test text.")
	}
}

func TestHandleSamples_TranscribeError(t *testing.T) {
	cfg := &config{Model: "base", WhisperBin: "/nonexistent/binary"}
	// Should log to stderr and return cleanly, not panic.
	handleSamples(silentSamples(sampleRate), cfg)
}

func TestHandleSamples_EmptyTranscript(t *testing.T) {
	bin := whisperBin(t)
	t.Setenv("WHISPER_TRANSCRIPT", "") // whisper returns nothing

	written, restore := captureClipboard(t)
	defer restore()

	cfg := &config{Model: "base", WhisperBin: bin}
	handleSamples(silentSamples(sampleRate), cfg)

	// Nothing recognised → clipboard should not be written.
	if *written != "" {
		t.Errorf("clipboard should not be written for empty transcript, got %q", *written)
	}
}

// ---------------------------------------------------------------------------
// listDevices  (device-listing logic, mocking the PortAudio device call)
// ---------------------------------------------------------------------------

func TestListDevices_FormatsOutput(t *testing.T) {
	orig := paDevices
	defer func() { paDevices = orig }()

	paDevices = func() ([]*portaudio.DeviceInfo, error) {
		return []*portaudio.DeviceInfo{
			{Name: "Built-in Mic", MaxInputChannels: 1},
			{Name: "HDMI Out", MaxInputChannels: 0},   // output-only, should be excluded
			{Name: "USB Headset", MaxInputChannels: 2}, //nolint
		}, nil
	}

	out, err := listDevices()
	if err != nil {
		t.Fatalf("listDevices: %v", err)
	}
	if !strings.Contains(out, "Built-in Mic") {
		t.Errorf("output should contain 'Built-in Mic', got:\n%s", out)
	}
	if strings.Contains(out, "HDMI Out") {
		t.Errorf("output-only device should be excluded, got:\n%s", out)
	}
	if !strings.Contains(out, "USB Headset") {
		t.Errorf("output should contain 'USB Headset', got:\n%s", out)
	}
}

func TestListDevices_PropagatesError(t *testing.T) {
	orig := paDevices
	defer func() { paDevices = orig }()
	paDevices = func() ([]*portaudio.DeviceInfo, error) {
		return nil, errors.New("portaudio error")
	}

	_, err := listDevices()
	if err == nil {
		t.Error("expected error when paDevices fails, got nil")
	}
}

// ---------------------------------------------------------------------------
// realOpenAudioStream  (PortAudio device-selection logic, mocking the device layer)
// ---------------------------------------------------------------------------

func TestRealOpenAudioStream_InvalidDeviceIndex(t *testing.T) {
	orig := paDevices
	defer func() { paDevices = orig }()
	paDevices = func() ([]*portaudio.DeviceInfo, error) {
		return make([]*portaudio.DeviceInfo, 2), nil // 2 devices: indices 0 and 1
	}

	rec := &recorder{}
	cfg := &config{Device: 5} // out of range
	_, err := realOpenAudioStream(rec, cfg)
	if err == nil {
		t.Error("expected error for out-of-range device index, got nil")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error should mention 'out of range', got: %v", err)
	}
}

func TestRealOpenAudioStream_DeviceListError(t *testing.T) {
	orig := paDevices
	defer func() { paDevices = orig }()
	paDevices = func() ([]*portaudio.DeviceInfo, error) {
		return nil, fmt.Errorf("portaudio initialisation failed")
	}

	rec := &recorder{}
	cfg := &config{Device: 0}
	_, err := realOpenAudioStream(rec, cfg)
	if err == nil {
		t.Error("expected error when device listing fails, got nil")
	}
	if !strings.Contains(err.Error(), "list devices") {
		t.Errorf("error should wrap 'list devices', got: %v", err)
	}
}
