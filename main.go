package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"syscall/js"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

var type3HashKeys = []string{
	"FontBBox",
	"FontMatrix",
	"CharProcs",
	"Encoding",
	"Widths",
	"FirstChar",
	"LastChar",
	"Resources",
	"ToUnicode",
}

func ignoreStreamKey(key string) bool {
	switch key {
	case "Length", "Filter", "DecodeParms":
		return true
	default:
		return false
	}
}

func canonicalObject(ctx *model.Context, obj types.Object, depth int, stack map[string]bool) (string, error) {
	if obj == nil {
		return "null", nil
	}
	if depth > 40 {
		return "DEPTH_LIMIT", nil
	}

	if ir, ok := obj.(types.IndirectRef); ok {
		refKey := fmt.Sprintf("%d:%d", ir.ObjectNumber.Value(), ir.GenerationNumber.Value())
		if stack[refKey] {
			return "CYCLE", nil
		}
		stack[refKey] = true
		resolved, err := ctx.Dereference(ir)
		if err != nil {
			delete(stack, refKey)
			return "", err
		}
		result, err := canonicalObject(ctx, resolved, depth+1, stack)
		delete(stack, refKey)
		return result, err
	}

	switch v := obj.(type) {
	case types.StreamDict:
		if err := v.Decode(); err != nil {
			v.Content = v.Raw
		}
		keys := make([]string, 0, len(v.Dict))
		for key := range v.Dict {
			if !ignoreStreamKey(key) {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)

		var b strings.Builder
		b.WriteString("STREAM{")
		for _, key := range keys {
			childString, err := canonicalObject(ctx, v.Dict[key], depth+1, stack)
			if err != nil {
				return "", err
			}
			b.WriteString(key)
			b.WriteString("=")
			b.WriteString(childString)
			b.WriteString(";")
		}
		b.WriteString("}DATA=")
		sum := sha256.Sum256(v.Content)
		b.WriteString(hex.EncodeToString(sum[:]))
		return b.String(), nil

	case types.Dict:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		var b strings.Builder
		b.WriteString("DICT{")
		for _, key := range keys {
			childString, err := canonicalObject(ctx, v[key], depth+1, stack)
			if err != nil {
				return "", err
			}
			b.WriteString(key)
			b.WriteString("=")
			b.WriteString(childString)
			b.WriteString(";")
		}
		b.WriteString("}")
		return b.String(), nil

	case types.Array:
		var b strings.Builder
		b.WriteString("ARRAY[")
		for _, child := range v {
			childString, err := canonicalObject(ctx, child, depth+1, stack)
			if err != nil {
				return "", err
			}
			b.WriteString(childString)
			b.WriteString(",")
		}
		b.WriteString("]")
		return b.String(), nil
	}

	return fmt.Sprintf("%T:%v", obj, obj), nil
}

func type3Signature(ctx *model.Context, fontDict types.Dict) (string, string, error) {
	var b strings.Builder
	b.WriteString("Subtype=Type3;")

	for _, key := range type3HashKeys {
		obj, found := fontDict.Find(key)
		b.WriteString(key)
		b.WriteString("=")
		if !found || obj == nil {
			b.WriteString("(missing);")
			continue
		}

		value, err := canonicalObject(ctx, obj, 0, map[string]bool{})
		if err != nil {
			return "", "", err
		}
		b.WriteString(value)
		b.WriteString(";")
	}

	signature := b.String()
	sum := sha256.Sum256([]byte(signature))
	return signature, hex.EncodeToString(sum[:]), nil
}

func filterNames(sd *types.StreamDict) string {
	if sd == nil || len(sd.FilterPipeline) == 0 {
		return "(none)"
	}

	names := make([]string, 0, len(sd.FilterPipeline))
	for _, f := range sd.FilterPipeline {
		names = append(names, f.Name)
	}
	return strings.Join(names, ",")
}

func imageDimension(ctx *model.Context, sd *types.StreamDict, key string) int {
	if sd == nil {
		return 0
	}
	obj, found := sd.Find(key)
	if !found || obj == nil {
		return 0
	}
	i, err := ctx.DereferenceInteger(obj)
	if err != nil || i == nil {
		return 0
	}
	return i.Value()
}

func imageColorSpace(sd *types.StreamDict) string {
	if sd == nil {
		return "(none)"
	}
	obj, found := sd.Find("ColorSpace")
	if !found || obj == nil {
		return "(none)"
	}
	return fmt.Sprintf("%v", obj)
}

func scanImages(ctx *model.Context) error {
	if ctx == nil || ctx.XRefTable == nil {
		return fmt.Errorf("missing PDF context")
	}
	if err := ctx.EnsurePageCount(); err != nil {
		return err
	}

	fmt.Println("[Image Scan] Start")
	fmt.Println("[Image Scan] page count:", ctx.PageCount)

	seenUnique := map[string]bool{}
	uniqueCount := 0
	pageRefs := 0
	totalRawBytes := 0

	for pageNr := 1; pageNr <= ctx.PageCount; pageNr++ {
		pageDict, _, _, err := ctx.PageDict(pageNr, true)
		if err != nil || pageDict == nil {
			continue
		}

		resObj, found := pageDict.Find("Resources")
		if !found || resObj == nil {
			continue
		}
		resDict, err := ctx.DereferenceDict(resObj)
		if err != nil || resDict == nil {
			continue
		}

		xObj, found := resDict.Find("XObject")
		if !found || xObj == nil {
			continue
		}
		xObjects, err := ctx.DereferenceDict(xObj)
		if err != nil || xObjects == nil {
			continue
		}

		for resourceName, obj := range xObjects {
			sd, _, err := ctx.DereferenceStreamDict(obj)
			if err != nil || sd == nil {
				continue
			}

			subtypeObj, found := sd.Find("Subtype")
			if !found || subtypeObj == nil {
				continue
			}
			subtypeName, ok := subtypeObj.(types.Name)
			if !ok || string(subtypeName) != "Image" {
				continue
			}

			pageRefs++

			refKey := fmt.Sprintf("%v", obj)
			if !seenUnique[refKey] {
				seenUnique[refKey] = true
				uniqueCount++
				totalRawBytes += len(sd.Raw)
			}

			width := imageDimension(ctx, sd, "Width")
			height := imageDimension(ctx, sd, "Height")
			bpc := 0
			if p := sd.IntEntry("BitsPerComponent"); p != nil {
				bpc = *p
			}
			hasSMask := false
			if sm, found := sd.Find("SMask"); found && sm != nil {
				hasSMask = true
			}

			fmt.Printf(
				"[Image Scan] page=%d resource=%s ref=%s width=%d height=%d filter=%s colorSpace=%s bpc=%d rawBytes=%d contentBytes=%d sMask=%t\n",
				pageNr,
				resourceName,
				refKey,
				width,
				height,
				filterNames(sd),
				imageColorSpace(sd),
				bpc,
				len(sd.Raw),
				len(sd.Content),
				hasSMask,
			)
		}
	}

	fmt.Println("[Image Scan] =================================")
	fmt.Println("[Image Scan] page image refs:", pageRefs)
	fmt.Println("[Image Scan] unique image objects:", uniqueCount)
	fmt.Println("[Image Scan] unique raw image bytes:", totalRawBytes)
	fmt.Printf("[Image Scan] unique raw image MiB: %.2f\n", float64(totalRawBytes)/(1024*1024))
	fmt.Println("[Image Scan] Finished")

	return nil
}

func deduplicateType3Fonts(ctx *model.Context) (int, int, int, error) {
	if ctx == nil || ctx.XRefTable == nil {
		return 0, 0, 0, fmt.Errorf("missing PDF context")
	}
	if err := ctx.EnsurePageCount(); err != nil {
		return 0, 0, 0, err
	}

	fmt.Println("[Type3 Dedup] Start")
	fmt.Println("[Type3 Dedup] page count:", ctx.PageCount)

	representativeBySignature := map[string]types.IndirectRef{}
	hashBySignature := map[string]string{}
	groupCounts := map[string]int{}
	seenOriginalRefs := map[string]bool{}
	totalType3 := 0
	replaced := 0

	for pageNr := 1; pageNr <= ctx.PageCount; pageNr++ {
		pageDict, _, _, err := ctx.PageDict(pageNr, true)
		if err != nil || pageDict == nil {
			continue
		}

		resObj, found := pageDict.Find("Resources")
		if !found || resObj == nil {
			continue
		}
		resDict, err := ctx.DereferenceDict(resObj)
		if err != nil || resDict == nil {
			continue
		}

		fontObj, found := resDict.Find("Font")
		if !found || fontObj == nil {
			continue
		}
		fontResources, err := ctx.DereferenceDict(fontObj)
		if err != nil || fontResources == nil {
			continue
		}

		for resourceName, fontObj := range fontResources {
			fontRef, ok := fontObj.(types.IndirectRef)
			if !ok {
				continue
			}

			refKey := fmt.Sprintf("%d:%d", fontRef.ObjectNumber.Value(), fontRef.GenerationNumber.Value())
			fontDict, err := ctx.DereferenceDict(fontRef)
			if err != nil || fontDict == nil {
				continue
			}

			subtypeObj, found := fontDict.Find("Subtype")
			if !found || subtypeObj == nil {
				continue
			}
			subtypeName, ok := subtypeObj.(types.Name)
			if !ok || string(subtypeName) != "Type3" {
				continue
			}

			if !seenOriginalRefs[refKey] {
				totalType3++
				seenOriginalRefs[refKey] = true
			}

			signature, hashValue, err := type3Signature(ctx, fontDict)
			if err != nil {
				return totalType3, replaced, len(representativeBySignature), err
			}

			groupCounts[signature]++
			hashBySignature[signature] = hashValue

			if representative, found := representativeBySignature[signature]; found {
				if representative.ObjectNumber.Value() != fontRef.ObjectNumber.Value() || representative.GenerationNumber.Value() != fontRef.GenerationNumber.Value() {
					fontResources[resourceName] = representative
					replaced++
				}
				continue
			}

			representativeBySignature[signature] = fontRef
		}
	}

	type groupInfo struct {
		hash  string
		count int
	}
	groups := make([]groupInfo, 0)
	for signature, count := range groupCounts {
		if count > 1 {
			groups = append(groups, groupInfo{hash: hashBySignature[signature], count: count})
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].count > groups[j].count })

	fmt.Println("[Type3 Dedup] =================================")
	fmt.Println("[Type3 Dedup] total Type3 objects:", totalType3)
	fmt.Println("[Type3 Dedup] unique signatures:", len(representativeBySignature))
	fmt.Println("[Type3 Dedup] resource refs replaced:", replaced)
	fmt.Println("[Type3 Dedup] duplicate groups:", len(groups))

	maxDisplay := 20
	if len(groups) < maxDisplay {
		maxDisplay = len(groups)
	}
	for i := 0; i < maxDisplay; i++ {
		shortHash := groups[i].hash
		if len(shortHash) > 16 {
			shortHash = shortHash[:16]
		}
		fmt.Printf("[Type3 Dedup] #%d count=%d hash=%s\n", i+1, groups[i].count, shortHash)
	}
	fmt.Println("[Type3 Dedup] Finished")

	return totalType3, replaced, len(representativeBySignature), nil
}

func optimizePDF(this js.Value, args []js.Value) interface{} {
	if len(args) < 1 {
		return map[string]interface{}{"ok": false, "error": "Missing PDF input."}
	}
	input := args[0]
	if input.Type() != js.TypeObject {
		return map[string]interface{}{"ok": false, "error": "Invalid PDF input."}
	}

	length := input.Get("length").Int()
	if length <= 0 {
		return map[string]interface{}{"ok": false, "error": "PDF input is empty."}
	}

	inputBytes := make([]byte, length)
	copied := js.CopyBytesToGo(inputBytes, input)
	if copied != length {
		return map[string]interface{}{
			"ok": false,
			"error": fmt.Sprintf("Failed to copy PDF bytes. expected=%d copied=%d", length, copied),
		}
	}

	conf := model.NewDefaultConfiguration()
	conf.Cmd = model.OPTIMIZE
	conf.Optimize = true
	conf.OptimizeResourceDicts = true
	conf.OptimizeDuplicateContentStreams = true

	reader := bytes.NewReader(inputBytes)
	ctx, err := api.ReadValidateAndOptimize(reader, conf)
	if err != nil {
		return map[string]interface{}{
			"ok": false,
			"error": fmt.Sprintf("pdfcpu prepare failed: %v", err),
		}
	}

	if err := scanImages(ctx); err != nil {
		fmt.Println("[Image Scan] Error:", err)
	}

	totalType3, replaced, uniqueType3, err := deduplicateType3Fonts(ctx)
	if err != nil {
		return map[string]interface{}{
			"ok": false,
			"error": fmt.Sprintf("Type3 dedup failed: %v", err),
		}
	}

	var output bytes.Buffer
	if err := api.WriteContext(ctx, &output); err != nil {
		return map[string]interface{}{
			"ok": false,
			"error": fmt.Sprintf("pdfcpu write failed: %v", err),
		}
	}

	outputBytes := output.Bytes()
	if len(outputBytes) <= 0 {
		return map[string]interface{}{"ok": false, "error": "pdfcpu returned empty PDF."}
	}

	fmt.Println("[Type3 Dedup] Original bytes:", len(inputBytes))
	fmt.Println("[Type3 Dedup] Output bytes:", len(outputBytes))
	fmt.Printf("[Type3 Dedup] Saved: %.1f%%\n", float64(len(inputBytes)-len(outputBytes))/float64(len(inputBytes))*100)

	jsOutput := js.Global().Get("Uint8Array").New(len(outputBytes))
	copied = js.CopyBytesToJS(jsOutput, outputBytes)
	if copied != len(outputBytes) {
		return map[string]interface{}{
			"ok": false,
			"error": fmt.Sprintf("Failed to copy output PDF bytes. expected=%d copied=%d", len(outputBytes), copied),
		}
	}

	return map[string]interface{}{
		"ok":                true,
		"output":            jsOutput,
		"originalSize":      len(inputBytes),
		"outputSize":        len(outputBytes),
		"type3Total":        totalType3,
		"type3Unique":       uniqueType3,
		"type3RefsReplaced": replaced,
	}
}

func main() {
	api.DisableConfigDir()
	js.Global().Set("pdfcpuOptimize", js.FuncOf(optimizePDF))
	println("pdfcpu WASM ready - Type3 dedup + Image Scan")
	select {}
}
