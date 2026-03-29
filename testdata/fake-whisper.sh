#!/bin/sh
# Fake whisper binary used by unit tests.
#
# Behaviour is driven by environment variables set by the test:
#
#   WHISPER_TRANSCRIPT   Text to write to <outdir>/<basename>.txt  (normal path)
#   WHISPER_STDERR_TEXT  Text to write to stderr only; no .txt file created
#                        (exercises the timestamp-parsing fallback path)
#   WHISPER_EXIT_CODE    Exit code to return (default 0)

OUTPUT_DIR=''
INPUT=''
PREV=''

for arg in "$@"; do
    if [ "$PREV" = "--output_dir" ]; then OUTPUT_DIR="$arg"; fi
    PREV="$arg"
done

for arg in "$@"; do
    case "$arg" in
        --*) ;;
        *) [ -z "$INPUT" ] && INPUT="$arg" ;;
    esac
done

if [ -n "${WHISPER_STDERR_TEXT:-}" ]; then
    printf '%s\n' "$WHISPER_STDERR_TEXT" >&2
elif [ -n "$OUTPUT_DIR" ] && [ -n "$INPUT" ]; then
    BASENAME=$(basename "$INPUT" .wav)
    printf '%s' "${WHISPER_TRANSCRIPT:-}" > "$OUTPUT_DIR/${BASENAME}.txt"
fi

exit "${WHISPER_EXIT_CODE:-0}"
