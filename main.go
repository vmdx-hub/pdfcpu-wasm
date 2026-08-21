package main

import (
	"bytes"
	"syscall/js"

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

func optimizePDF(this js.Value, args []js.Value) interface{} {

	if len(args) < 1 {
		return map[string]interface{}{
			"ok":    false,
			"error": "missing input PDF",
		}
	}

	input := args[0]

	inputBytes := make([]byte, input.Get("length").Int())

	js.CopyBytesToGo(
		inputBytes,
		input,
	)

	reader := bytes.NewReader(
		inputBytes,
	)

	var output bytes.Buffer

	err := api.Optimize(
		reader,
		&output,
		nil,
	)

	if err != nil {
		return map[string]interface{}{
			"ok":    false,
			"error": err.Error(),
		}
	}

	outputBytes := output.Bytes()

	uint8Array := js.Global().
		Get("Uint8Array").
		New(len(outputBytes))

	js.CopyBytesToJS(
		uint8Array,
		outputBytes,
	)

	return map[string]interface{}{
		"ok":     true,
		"output": uint8Array,
	}
}

func main() {

	// ブラウザWASMでは /tmp 等の設定フォルダを作れないため、
	// pdfcpuの設定ディレクトリ機能を無効化する。
	api.DisableConfigDir()

	js.Global().Set(
		"pdfcpuOptimize",
		js.FuncOf(optimizePDF),
	)

	println("pdfcpu WASM ready")

	select {}
}
