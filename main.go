package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	"sort"
	"strings"
	"syscall/js"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/filter"
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

const (
	jpegQuality         = 85
	minImageRawBytes    = 500 * 1024
	minImageWidth       = 1000
	minImageHeight      = 700
)

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

func imageComponents(ctx *model.Context, sd *types.StreamDict) (int, string) {
	if sd == nil {
		return 0, "(none)"
	}

	obj, found := sd.Find("ColorSpace")
	if !found || obj == nil {
		return 0, "(none)"
	}

	resolved, err := ctx.Dereference(obj)
	if err != nil || resolved == nil {
		return 0, fmt.Sprintf("%v", obj)
	}

	switch cs := resolved.(type) {
	case types.Name:
		name := string(cs)
		switch name {
		case "DeviceRGB":
			return 3, name
		case "DeviceGray":
			return 1, name
		case "DeviceCMYK":
			return 4, name
		default:
			return 0, name
		}

	case types.Array:
		if len(cs) >= 2 {
			if n, ok := cs[0].(types.Name); ok && string(n) == "ICCBased" {
				profile, _, err := ctx.DereferenceStreamDict(cs[1])
				if err == nil && profile != nil {
					if p := profile.IntEntry("N"); p != nil {
						return *p, "ICCBased"
					}
				}
				return 0, "ICCBased"
			}
		}
		return 0, fmt.Sprintf("%v", cs)
	}

	return 0, fmt.Sprintf("%v", resolved)
}

func hasSoftMask(sd *types.StreamDict) bool {
	if sd == nil {
		return false
	}
	sm, found := sd.Find("SMask")
	return found && sm != nil
}

func reencodeLargeFlateImages(ctx *model.Context) (int, int64, int64, error) {
	if ctx == nil || ctx.XRefTable == nil {
		return 0, 0, 0, fmt.Errorf("missing PDF context")
	}
	if err := ctx.EnsurePageCount(); err != nil {
		return 0, 0, 0, err
	}

	fmt.Println("[Image Reencode] Start")
	fmt.Println("[Image Reencode] quality:", jpegQuality)

	seen := map[string]bool{}
	reencoded := 0
	var beforeTotal int64
	var afterTotal int64

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
			refKey := fmt.Sprintf("%v", obj)
			if seen[refKey] {
				continue
			}
			seen[refKey] = true

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

			if !sd.HasSoleFilterNamed(filter.Flate) {
				continue
			}
			if hasSoftMask(sd) {
				continue
			}

			width := imageDimension(ctx, sd, "Width")
			height := imageDimension(ctx, sd, "Height")
			if width < minImageWidth || height < minImageHeight {
				continue
			}
			if len(sd.Raw) < minImageRawBytes {
				continue
			}

			bpc := 0
			if p := sd.IntEntry("BitsPerComponent"); p != nil {
				bpc = *p
			}
			if bpc != 8 {
				continue
			}

			components, csName := imageComponents(ctx, sd)
			if components != 3 {
				fmt.Printf("[Image Reencode] skip page=%d resource=%s ref=%s components=%d colorSpace=%s\n", pageNr, resourceName, refKey, components, csName)
				continue
			}

			before := len(sd.Raw)
			if err := sd.Decode(); err != nil {
				fmt.Printf("[Image Reencode] decode failed page=%d ref=%s err=%v\n", pageNr, refKey, err)
				continue
			}

			expected := width * height * 3
			if len(sd.Content) != expected {
				fmt.Printf("[Image Reencode] skip unexpected decoded size page=%d ref=%s got=%d expected=%d\n", pageNr, refKey, len(sd.Content), expected)
				continue
			}

			img := image.NewRGBA(image.Rect(0, 0, width, height))
			src := sd.Content
			dst := img.Pix
			si := 0
			di := 0
			for si < len(src) {
				dst[di] = src[si]
				dst[di+1] = src[si+1]
				dst[di+2] = src[si+2]
				dst[di+3] = 255
				si += 3
				di += 4
			}

			var jpg bytes.Buffer
			if err := jpeg.Encode(&jpg, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
				fmt.Printf("[Image Reencode] jpeg encode failed page=%d ref=%s err=%v\n", pageNr, refKey, err)
				continue
			}

			jpgBytes := jpg.Bytes()
			if len(jpgBytes) >= before {
				fmt.Printf("[Image Reencode] skip no gain page=%d ref=%s before=%d jpeg=%d\n", pageNr, refKey, before, len(jpgBytes))
				continue
			}

			sd.Raw = append([]byte(nil), jpgBytes...)
			sd.Content = nil
			sd.FilterPipeline = []types.PDFFilter{{Name: filter.DCT, DecodeParms: nil}}
			sd.CSComponents = 3
			sd.Update("Filter", types.Name(filter.DCT))
			delete(sd.Dict, "DecodeParms")
			sd.Update("ColorSpace", types.Name("DeviceRGB"))
			streamLength := int64(len(sd.Raw))
			sd.StreamLength = &streamLength
			sd.Update("Length", types.Integer(streamLength))

			reencoded++
			beforeTotal += int64(before)
			afterTotal += int64(len(jpgBytes))

			fmt.Printf(
				"[Image Reencode] page=%d resource=%s ref=%s %dx%d %s before=%d jpeg=%d saved=%.1f%%\n",
				pageNr,
				resourceName,
				refKey,
				width,
				height,
				csName,
				before,
				len(jpgBytes),
				float64(before-len(jpgBytes))/float64(before)*100,
			)
		}
	}

	fmt.Println("[Image Reencode] =================================")
	fmt.Println("[Image Reencode] images reencoded:", reencoded)
	fmt.Println("[Image Reencode] before bytes:", beforeTotal)
	fmt.Println("[Image Reencode] after bytes:", afterTotal)
	if beforeTotal > 0 {
		fmt.Printf("[Image Reencode] saved: %.1f%%\n", float64(beforeTotal-afterTotal)/float64(beforeTotal)*100)
	}
	fmt.Println("[Image Reencode] Finished")

	return reencoded, beforeTotal, afterTotal, nil
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

	imagesReencoded, imageBefore, imageAfter, err := reencodeLargeFlateImages(ctx)
	if err != nil {
		return map[string]interface{}{
			"ok": false,
			"error": fmt.Sprintf("Image reencode failed: %v", err),
		}
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

	fmt.Println("[Final] Original bytes:", len(inputBytes))
	fmt.Println("[Final] Output bytes:", len(outputBytes))
	fmt.Printf("[Final] Saved: %.1f%%\n", float64(len(inputBytes)-len(outputBytes))/float64(len(inputBytes))*100)

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
		"imagesReencoded":   imagesReencoded,
		"imageBytesBefore":  imageBefore,
		"imageBytesAfter":   imageAfter,
	}
}

func main() {
	api.DisableConfigDir()
	js.Global().Set("pdfcpuOptimize", js.FuncOf(optimizePDF))
	println("pdfcpu WASM ready - Type3 dedup + JPEG image reencode")
	select {}
}
