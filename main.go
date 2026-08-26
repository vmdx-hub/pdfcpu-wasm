package main

import (
	"bytes"
	"fmt"
	"syscall/js"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

func optimizePDF(this js.Value, args []js.Value) interface{} {

	// ========================================
	// 引数チェック
	// ========================================

	if len(args) < 1 {
		return map[string]interface{}{
			"ok":    false,
			"error": "Missing PDF input.",
		}
	}

	input :=
		args[0]

	if input.Type() != js.TypeObject {
		return map[string]interface{}{
			"ok":    false,
			"error": "Invalid PDF input.",
		}
	}

	length :=
		input.Get("length").Int()

	if length <= 0 {
		return map[string]interface{}{
			"ok":    false,
			"error": "PDF input is empty.",
		}
	}

	// ========================================
	// JS Uint8Array → Go []byte
	// ========================================

	inputBytes :=
		make(
			[]byte,
			length,
		)

	copied :=
		js.CopyBytesToGo(
			inputBytes,
			input,
		)

	if copied != length {
		return map[string]interface{}{
			"ok": false,
			"error": fmt.Sprintf(
				"Failed to copy PDF bytes. expected=%d copied=%d",
				length,
				copied,
			),
		}
	}

	reader :=
		bytes.NewReader(
			inputBytes,
		)

	var output bytes.Buffer

	// ========================================
	// pdfcpu Configuration
	// ========================================

	conf :=
		model.NewDefaultConfiguration()

	// 通常の内部最適化
	conf.Optimize = true

	// ページContent Streamを解析して
	// 未使用Font / Image等をResource Dictionaryから除去
	conf.OptimizeResourceDicts = true

	// ページ間で重複しているContent Streamの検出・削除
	// デフォルトはfalseなので明示的にON
	conf.OptimizeDuplicateContentStreams = true

	// ========================================
	// pdfcpu Optimize
	// ========================================

	err :=
		api.Optimize(
			reader,
			&output,
			conf,
		)

	if err != nil {
		return map[string]interface{}{
			"ok": false,
			"error": fmt.Sprintf(
				"pdfcpu Optimize failed: %v",
				err,
			),
		}
	}

	outputBytes :=
		output.Bytes()

	if len(outputBytes) <= 0 {
		return map[string]interface{}{
			"ok":    false,
			"error": "pdfcpu returned empty PDF.",
		}
	}

	// ========================================
	// Go []byte → JS Uint8Array
	// ========================================

	jsOutput :=
		js.Global().
			Get("Uint8Array").
			New(
				len(outputBytes),
			)

	copied =
		js.CopyBytesToJS(
			jsOutput,
			outputBytes,
		)

	if copied != len(outputBytes) {
		return map[string]interface{}{
			"ok": false,
			"error": fmt.Sprintf(
				"Failed to copy output PDF bytes. expected=%d copied=%d",
				len(outputBytes),
				copied,
			),
		}
	}

	// ========================================
// 戻り値
// ========================================

return map[string]interface{}{
    "ok":           true,
    "output":       jsOutput,
    "originalSize": len(inputBytes),
    "outputSize":   len(outputBytes),
}
}

func main() {

    // ブラウザWASMでは設定ディレクトリを
    // 使用できないため無効化
    api.DisableConfigDir()

    js.Global().Set(
        "pdfcpuOptimize",
        js.FuncOf(optimizePDF),
    )

    println("pdfcpu WASM ready - enhanced optimize")

    select {}
}
