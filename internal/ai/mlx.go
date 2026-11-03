package ai

import _ "embed"

// MLXWorker is the script berthd runs for provider "mlx": a model on the
// Mac's GPU whose neurons apps can read and pin.
//
//go:embed mlx_worker.py
var MLXWorker []byte

// MLXPackage is what the worker's Python environment installs. Pinned,
// because the worker reaches into the model's layers.
const MLXPackage = "mlx-lm==0.32.2"

// DefaultMLXModel is the model berth ai:mlx serves unless told otherwise:
// the same Qwen3 8B that ai:setup gives Ollama, quantized to 4 bits (about
// 5 GB).
const DefaultMLXModel = "mlx-community/Qwen3-8B-4bit"
