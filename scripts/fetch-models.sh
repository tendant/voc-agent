#!/usr/bin/env bash
# Downloads the local speech models into ./models (gitignored). Idempotent.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p models
cd models

BASE=https://github.com/k2-fsa/sherpa-onnx/releases/download

fetch_tar() { # <url> <dir-it-extracts-to>
  if [[ -d "$2" ]]; then echo "ok   $2"; return; fi
  echo "get  $2"
  curl -fsSL --retry 3 "$1" | tar xjf -
}

fetch_file() { # <url> <file>
  if [[ -f "$2" ]]; then echo "ok   $2"; return; fi
  echo "get  $2"
  curl -fsSL --retry 3 -o "$2.part" "$1" && mv "$2.part" "$2"
}

# ASR: SenseVoiceSmall (zh/en/ja/ko/yue), int8
fetch_tar "$BASE/asr-models/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17.tar.bz2" \
  sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17
# VAD: Silero
fetch_file "$BASE/asr-models/silero_vad.onnx" silero_vad.onnx
# TTS: Piper voice
fetch_tar "$BASE/tts-models/vits-piper-en_US-lessac-medium.tar.bz2" vits-piper-en_US-lessac-medium

echo "models ready in $(pwd)"
