.PHONY: build test test-x11 clean

build:
	go build -o whisper-stt .

# Unit tests exclude the gohook import with -tags noX11 so they run in any
# headless environment without needing an X display.
test:
	go test -tags noX11 -race -count=1 ./...

# Integration/manual smoke-test that exercises the real gohook X11 code.
# Requires a running X server (DISPLAY must be set, or Xvfb is started).
test-x11:
	@if [ -z "$$DISPLAY" ]; then \
		command -v Xvfb >/dev/null 2>&1 || { echo "Install Xvfb: sudo apt install xvfb"; exit 1; }; \
		Xvfb :99 -screen 0 1024x768x24 & \
		XVFB_PID=$$!; \
		sleep 0.5; \
		DISPLAY=:99 go test -race -count=1 ./... ; \
		STATUS=$$?; \
		kill $$XVFB_PID 2>/dev/null || true; \
		exit $$STATUS; \
	else \
		go test -race -count=1 ./...; \
	fi

clean:
	rm -f whisper-stt
