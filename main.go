package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"syscall/js"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// ========================================
// Type3診断
// ========================================

func analyzeType3Fonts(
	inputBytes []byte,
	conf *model.Configuration,
) {

	fmt.Println("[Type3 Scan] Start")

	reader := bytes.NewReader(inputBytes)

	ctx, err := api.ReadContext(reader, conf)
	if err != nil {
		fmt.Println(
			"[Type3 Scan] ReadContext failed:",
			err,
		)
		return
	}

	if ctx == nil || ctx.XRefTable == nil {
		fmt.Println(
			"[Type3 Scan] No XRefTable.",
		)
		return
	}

	totalType3 := 0

	baseFontCounts :=
		map[string]int{}

	objectNumbers :=
		make(
			[]int,
			0,
		)

	// ========================================
	// XRefTable全走査
	// ========================================

	for objectNumber, entry :=
		range ctx.XRefTable.Table {

		if entry == nil ||
			entry.Free ||
			entry.Object == nil {

			continue
		}

		dict, ok :=
			entry.Object.(types.Dict)

		if !ok {
			continue
		}

		// ========================================
		// /Type /Font
		// ========================================

		typeObj, found :=
			dict.Find("Type")

		if !found ||
			typeObj == nil {

			continue
		}

		typeName, ok :=
			typeObj.(types.Name)

		if !ok {
			continue
		}

		if string(typeName) !=
			"Font" {

			continue
		}

		// ========================================
		// /Subtype /Type3
		// ========================================

		subtypeObj, found :=
			dict.Find("Subtype")

		if !found ||
			subtypeObj == nil {

			continue
		}

		subtypeName, ok :=
			subtypeObj.(types.Name)

		if !ok {
			continue
		}

		if string(subtypeName) !=
			"Type3" {

			continue
		}

		totalType3++

		objectNumbers =
			append(
				objectNumbers,
				objectNumber,
			)

		// ========================================
		// BaseFont取得
		// ========================================

		baseFont :=
			"(none)"

		baseFontObj, found :=
			dict.Find("BaseFont")

		if found &&
			baseFontObj != nil {

			switch v :=
				baseFontObj.(type) {

			case types.Name:
				baseFont =
					string(v)

			default:
				baseFont =
					fmt.Sprintf(
						"%v",
						v,
					)
			}
		}

		baseFont =
			strings.TrimSpace(
				baseFont,
			)

		if baseFont == "" {
			baseFont =
				"(empty)"
		}

		baseFontCounts[
			baseFont
		]++
	}

	// ========================================
	// 結果
	// ========================================

	fmt.Println(
		"[Type3 Scan] total Type3 fonts:",
		totalType3,
	)

	fmt.Println(
		"[Type3 Scan] unique BaseFont names:",
		len(baseFontCounts),
	)

	// ========================================
	// 出現数順
	// ========================================

	type fontCount struct {
		name  string
		count int
	}

	list :=
		make(
			[]fontCount,
			0,
			len(baseFontCounts),
		)

	for name, count :=
		range baseFontCounts {

		list =
			append(
				list,
				fontCount{
					name:  name,
					count: count,
				},
			)
	}

	sort.Slice(
		list,
		func(i, j int) bool {

			if list[i].count ==
				list[j].count {

				return list[i].name <
					list[j].name
			}

			return list[i].count >
				list[j].count
		},
	)

	// ========================================
	// 上位30件
	// ========================================

	maxDisplay := 30

	if len(list) <
		maxDisplay {

		maxDisplay =
			len(list)
	}

	fmt.Println(
		"[Type3 Scan] Top BaseFont duplicates:",
	)

	for i := 0;
		i < maxDisplay;
		i++ {

		fmt.Printf(
			"[Type3 Scan] #%d count=%d BaseFont=%s\n",
			i+1,
			list[i].count,
			list[i].name,
		)
	}

	// ========================================
	// Type3 object番号
	// ========================================

	sort.Ints(
		objectNumbers,
	)

	maxObjects := 50

	if len(objectNumbers) <
		maxObjects {

		maxObjects =
			len(objectNumbers)
	}

	if maxObjects > 0 {

		fmt.Println(
			"[Type3 Scan] First Type3 object numbers:",
			objectNumbers[
				0:maxObjects
			],
		)
	}

	fmt.Println(
		"[Type3 Scan] Finished",
	)
}

// ========================================
// PDF Optimize
// ========================================

func optimizePDF(
	this js.Value,
	args []js.Value,
) interface{} {

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

	if input.Type() !=
		js.TypeObject {

		return map[string]interface{}{
			"ok":    false,
			"error": "Invalid PDF input.",
		}
	}

	length :=
		input.Get(
			"length",
		).Int()

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

	// ========================================
	// pdfcpu Configuration
	// ========================================

	conf :=
		model.NewDefaultConfiguration()

	conf.Optimize =
		true

	conf.OptimizeResourceDicts =
		true

	conf.OptimizeDuplicateContentStreams =
		true

	// ========================================
	// Type3診断
	// ========================================

	analyzeType3Fonts(
		inputBytes,
		conf,
	)

	// ========================================
	// Optimize
	// ========================================

	reader :=
		bytes.NewReader(
			inputBytes,
		)

	var output bytes.Buffer

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
			Get(
				"Uint8Array",
			).
			New(
				len(outputBytes),
			)

	copied =
		js.CopyBytesToJS(
			jsOutput,
			outputBytes,
		)

	if copied !=
		len(outputBytes) {

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

// ========================================
// main
// ========================================

func main() {

	api.DisableConfigDir()

	js.Global().Set(
		"pdfcpuOptimize",
		js.FuncOf(
			optimizePDF,
		),
	)

	println(
		"pdfcpu WASM ready - enhanced optimize + Type3 scan",
	)

	select {}
}
