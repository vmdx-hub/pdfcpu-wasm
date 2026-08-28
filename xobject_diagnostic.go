package main

import (
	"bytes"
	"fmt"
	"strings"
	"syscall/js"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

func diagnosticFilterNames(sd *types.StreamDict) string {
	if sd == nil || len(sd.FilterPipeline) == 0 {
		return "(none)"
	}

	names := make([]string, 0, len(sd.FilterPipeline))
	for _, f := range sd.FilterPipeline {
		names = append(names, f.Name)
	}
	return strings.Join(names, ",")
}

func diagnosticSubtype(sd *types.StreamDict) string {
	if sd == nil {
		return "(none)"
	}
	obj, found := sd.Find("Subtype")
	if !found || obj == nil {
		return "(none)"
	}
	if n, ok := obj.(types.Name); ok {
		return string(n)
	}
	return fmt.Sprintf("%v", obj)
}

func scanXObjectTree(ctx *model.Context, pageNr int, xObjects types.Dict, path string, depth int, seen map[string]bool) {
	if ctx == nil || xObjects == nil || depth > 12 {
		return
	}

	for resourceName, obj := range xObjects {
		refKey := fmt.Sprintf("%v", obj)
		location := resourceName
		if path != "" {
			location = path + "/" + resourceName
		}

		sd, _, err := ctx.DereferenceStreamDict(obj)
		if err != nil || sd == nil {
			fmt.Printf("[XObject Scan] page=%d depth=%d path=%s ref=%s stream=false err=%v\n", pageNr, depth, location, refKey, err)
			continue
		}

		subtype := diagnosticSubtype(sd)
		width := imageDimension(ctx, sd, "Width")
		height := imageDimension(ctx, sd, "Height")
		bpc := 0
		if p := sd.IntEntry("BitsPerComponent"); p != nil {
			bpc = *p
		}

		fmt.Printf(
			"[XObject Scan] page=%d depth=%d path=%s ref=%s subtype=%s width=%d height=%d filter=%s bpc=%d rawBytes=%d sMask=%t\n",
			pageNr,
			depth,
			location,
			refKey,
			subtype,
			width,
			height,
			diagnosticFilterNames(sd),
			bpc,
			len(sd.Raw),
			hasSoftMask(sd),
		)

		if subtype != "Form" {
			continue
		}

		if seen[refKey] {
			fmt.Printf("[XObject Scan] page=%d path=%s ref=%s form-cycle-or-repeat=true\n", pageNr, location, refKey)
			continue
		}
		seen[refKey] = true

		resObj, found := sd.Find("Resources")
		if !found || resObj == nil {
			fmt.Printf("[XObject Scan] page=%d path=%s ref=%s form-resources=none\n", pageNr, location, refKey)
			continue
		}
		resDict, err := ctx.DereferenceDict(resObj)
		if err != nil || resDict == nil {
			fmt.Printf("[XObject Scan] page=%d path=%s ref=%s form-resources-error=%v\n", pageNr, location, refKey, err)
			continue
		}

		nestedObj, found := resDict.Find("XObject")
		if !found || nestedObj == nil {
			fmt.Printf("[XObject Scan] page=%d path=%s ref=%s nested-xobjects=none\n", pageNr, location, refKey)
			continue
		}
		nested, err := ctx.DereferenceDict(nestedObj)
		if err != nil || nested == nil {
			fmt.Printf("[XObject Scan] page=%d path=%s ref=%s nested-xobjects-error=%v\n", pageNr, location, refKey, err)
			continue
		}

		scanXObjectTree(ctx, pageNr, nested, location, depth+1, seen)
	}
}

func runLatePageXObjectDiagnostic(inputBytes []byte) {
	conf := model.NewDefaultConfiguration()
	conf.Cmd = model.OPTIMIZE
	conf.Optimize = true
	conf.OptimizeResourceDicts = true
	conf.OptimizeDuplicateContentStreams = true

	ctx, err := api.ReadValidateAndOptimize(bytes.NewReader(inputBytes), conf)
	if err != nil {
		fmt.Println("[XObject Scan] prepare failed:", err)
		return
	}
	if err := ctx.EnsurePageCount(); err != nil {
		fmt.Println("[XObject Scan] page count failed:", err)
		return
	}

	startPage := 43
	if ctx.PageCount < startPage {
		startPage = 1
	}

	fmt.Println("[XObject Scan] =================================")
	fmt.Println("[XObject Scan] recursive diagnostic start")
	fmt.Println("[XObject Scan] page count:", ctx.PageCount)
	fmt.Println("[XObject Scan] start page:", startPage)

	for pageNr := startPage; pageNr <= ctx.PageCount; pageNr++ {
		pageDict, _, _, err := ctx.PageDict(pageNr, true)
		if err != nil || pageDict == nil {
			fmt.Printf("[XObject Scan] page=%d page-dict-error=%v\n", pageNr, err)
			continue
		}

		resObj, found := pageDict.Find("Resources")
		if !found || resObj == nil {
			fmt.Printf("[XObject Scan] page=%d page-resources=none\n", pageNr)
			continue
		}
		resDict, err := ctx.DereferenceDict(resObj)
		if err != nil || resDict == nil {
			fmt.Printf("[XObject Scan] page=%d page-resources-error=%v\n", pageNr, err)
			continue
		}

		xObj, found := resDict.Find("XObject")
		if !found || xObj == nil {
			fmt.Printf("[XObject Scan] page=%d page-xobjects=none\n", pageNr)
			continue
		}
		xObjects, err := ctx.DereferenceDict(xObj)
		if err != nil || xObjects == nil {
			fmt.Printf("[XObject Scan] page=%d page-xobjects-error=%v\n", pageNr, err)
			continue
		}

		fmt.Printf("[XObject Scan] page=%d top-level-count=%d\n", pageNr, len(xObjects))
		scanXObjectTree(ctx, pageNr, xObjects, "", 0, map[string]bool{})
	}

	fmt.Println("[XObject Scan] recursive diagnostic finished")
	fmt.Println("[XObject Scan] =================================")
}

func installDiagnosticWrapper() {
	for i := 0; i < 200; i++ {
		original := js.Global().Get("pdfcpuOptimize")
		if original.Type() == js.TypeFunction {
			wrapper := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
				if len(args) > 0 && args[0].Type() == js.TypeObject {
					length := args[0].Get("length").Int()
					if length > 0 {
						inputBytes := make([]byte, length)
						if js.CopyBytesToGo(inputBytes, args[0]) == length {
							runLatePageXObjectDiagnostic(inputBytes)
						}
					}
				}
				return original.Invoke(args...)
			})
			js.Global().Set("pdfcpuOptimize", wrapper)
			fmt.Println("pdfcpu diagnostic wrapper ready - recursive XObject scan")
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Println("[XObject Scan] failed to install diagnostic wrapper")
}

func init() {
	go installDiagnosticWrapper()
}
