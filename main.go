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
// Page → Resources → Font を辿る
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

	pageCount := ctx.PageCount

	fmt.Println(
		"[Type3 Scan] page count:",
		pageCount,
	)

	totalFontRefs := 0
	totalType3Refs := 0

	uniqueFontObjects := map[string]bool{}
	uniqueType3Objects := map[string]bool{}
	baseFontCounts := map[string]int{}
	subtypeCounts := map[string]int{}

	// ========================================
	// 各ページ
	// ========================================

	for pageNr := 1; pageNr <= pageCount; pageNr++ {

		pageDict, _, _, err :=
			ctx.PageDict(
				pageNr,
				true,
			)

		if err != nil {
			fmt.Printf(
				"[Type3 Scan] Page %d PageDict error: %v\n",
				pageNr,
				err,
			)
			continue
		}

		if pageDict == nil {
			fmt.Printf(
				"[Type3 Scan] Page %d missing PageDict\n",
				pageNr,
			)
			continue
		}

		// ========================================
		// Resources
		// ========================================

		resObj, found :=
			pageDict.Find(
				"Resources",
			)

		if !found || resObj == nil {
			fmt.Printf(
				"[Type3 Scan] Page %d has no Resources\n",
				pageNr,
			)
			continue
		}

		resDict, err :=
			ctx.DereferenceDict(
				resObj,
			)

		if err != nil {
			fmt.Printf(
				"[Type3 Scan] Page %d Resources error: %v\n",
				pageNr,
				err,
			)
			continue
		}

		if resDict == nil {
			continue
		}

		// ========================================
		// Font Resources
		// ========================================

		fontObj, found :=
			resDict.Find(
				"Font",
			)

		if !found || fontObj == nil {
			continue
		}

		fontResources, err :=
			ctx.DereferenceDict(
				fontObj,
			)

		if err != nil {
			fmt.Printf(
				"[Type3 Scan] Page %d Font Resources error: %v\n",
				pageNr,
				err,
			)
			continue
		}

		if fontResources == nil {
			continue
		}

		pageFontCount := 0
		pageType3Count := 0

		// ========================================
		// F1 / F2 / F3 ...
		// ========================================

		for resourceName, fontRef := range fontResources {

			totalFontRefs++
			pageFontCount++

			fontKey :=
				fmt.Sprintf(
					"%v",
					fontRef,
				)

			uniqueFontObjects[fontKey] = true

			// ========================================
			// Font Dictionary
			// ========================================

			fontDict, err :=
				ctx.DereferenceDict(
					fontRef,
				)

			if err != nil {
				fmt.Printf(
					"[Type3 Scan] Page %d Font %s dereference error: %v\n",
					pageNr,
					resourceName,
					err,
				)
				continue
			}

			if fontDict == nil {
				continue
			}

			// ========================================
			// Subtype
			// ========================================

			subtype := "(none)"

			subtypeObj, found :=
				fontDict.Find(
					"Subtype",
				)

			if found && subtypeObj != nil {

				switch v := subtypeObj.(type) {

				case types.Name:
					subtype = string(v)

				default:
					subtype = fmt.Sprintf(
						"%v",
						v,
					)
				}
			}

			subtype =
				strings.TrimSpace(
					subtype,
				)

			if subtype == "" {
				subtype = "(empty)"
			}

			subtypeCounts[subtype]++

			// ========================================
			// Type3のみ
			// ========================================

			if subtype != "Type3" {
				continue
			}

			totalType3Refs++
			pageType3Count++

			uniqueType3Objects[fontKey] = true

			// ========================================
			// BaseFont
			// ========================================

			baseFont := "(none)"

			baseFontObj, found :=
				fontDict.Find(
					"BaseFont",
				)

			if found && baseFontObj != nil {

				switch v := baseFontObj.(type) {

				case types.Name:
					baseFont = string(v)

				default:
					baseFont = fmt.Sprintf(
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
				baseFont = "(empty)"
			}

			baseFontCounts[baseFont]++

			// ========================================
			// 最初の20件だけ詳細表示
			// ========================================

			if totalType3Refs <= 20 {
				fmt.Printf(
					"[Type3 Scan] Page=%d Resource=%s Ref=%s BaseFont=%s\n",
					pageNr,
					resourceName,
					fontKey,
					baseFont,
				)
			}
		}

		if pageFontCount > 0 {
			fmt.Printf(
				"[Type3 Scan] Page %d fonts=%d type3=%d\n",
				pageNr,
				pageFontCount,
				pageType3Count,
			)
		}
	}

	// ========================================
	// 集計用
	// ========================================

	type countItem struct {
		name  string
		count int
	}

	// ========================================
	// Subtype集計
	// ========================================

	subtypeList :=
		make(
			[]countItem,
			0,
			len(subtypeCounts),
		)

	for name, count := range subtypeCounts {
		subtypeList =
			append(
				subtypeList,
				countItem{
					name:  name,
					count: count,
				},
			)
	}

	sort.Slice(
		subtypeList,
		func(i, j int) bool {
			return subtypeList[i].count > subtypeList[j].count
		},
	)

	fmt.Println(
		"[Type3 Scan] Font subtype summary:",
	)

	for _, item := range subtypeList {
		fmt.Printf(
			"[Type3 Scan] subtype=%s refs=%d\n",
			item.name,
			item.count,
		)
	}

	// ========================================
	// BaseFont集計
	// ========================================

	baseFontList :=
		make(
			[]countItem,
			0,
			len(baseFontCounts),
		)

	for name, count := range baseFontCounts {
		baseFontList =
			append(
				baseFontList,
				countItem{
					name:  name,
					count: count,
				},
			)
	}

	sort.Slice(
		baseFontList,
		func(i, j int) bool {
			return baseFontList[i].count > baseFontList[j].count
		},
	)

	// ========================================
	// 最終結果
	// ========================================

	fmt.Println(
		"[Type3 Scan] =================================",
	)

	fmt.Println(
		"[Type3 Scan] total font resource refs:",
		totalFontRefs,
	)

	fmt.Println(
		"[Type3 Scan] unique font objects:",
		len(uniqueFontObjects),
	)

	fmt.Println(
		"[Type3 Scan] total Type3 resource refs:",
		totalType3Refs,
	)

	fmt.Println(
		"[Type3 Scan] unique Type3 objects:",
		len(uniqueType3Objects),
	)

	fmt.Println(
		"[Type3 Scan] unique Type3 BaseFont names:",
		len(baseFontCounts),
	)

	fmt.Println(
		"[Type3 Scan] Top Type3 BaseFonts:",
	)

	maxDisplay := 30

	if len(baseFontList) < maxDisplay {
		maxDisplay = len(baseFontList)
	}

	for i := 0; i < maxDisplay; i++ {
		fmt.Printf(
			"[Type3 Scan] #%d count=%d BaseFont=%s\n",
			i+1,
			baseFontList[i].count,
			baseFontList[i].name,
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

	input := args[0]

	if input.Type() != js.TypeObject {
		return map[string]interface{}{
			"ok":    false,
			"error": "Invalid PDF input.",
		}
	}

	length := input.Get("length").Int()

	if length <= 0 {
		return map[string]interface{}{
			"ok":    false,
			"error": "PDF input is empty.",
		}
	}

	// ========================================
	// JS → Go
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

	conf.Optimize = true
	conf.OptimizeResourceDicts = true
	conf.OptimizeDuplicateContentStreams = true

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

	outputBytes := output.Bytes()

	if len(outputBytes) <= 0 {
		return map[string]interface{}{
			"ok":    false,
			"error": "pdfcpu returned empty PDF.",
		}
	}

	// ========================================
	// Go → JS
	// ========================================

	jsOutput :=
		js.Global().
			Get("Uint8Array").
			New(len(outputBytes))

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
		js.FuncOf(optimizePDF),
	)

	println(
		"pdfcpu WASM ready - page resource Type3 scan v2",
	)

	select {}
}
