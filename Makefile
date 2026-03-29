.PHONY: build test clean

build:
	go build -o whisper-stt .

# gohook's C init code opens the X display at library-load time, so tests
# require a running X server.  Use Xvfb if DISPLAY is not already set.
test:
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
