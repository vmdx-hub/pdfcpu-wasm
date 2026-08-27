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

func hashType3Font(ctx *model.Context, fontDict types.Dict) (string, error) {
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
			return "", err
		}
		b.WriteString(value)
		b.WriteString(";")
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}

func analyzeType3Hashes(inputBytes []byte, conf *model.Configuration) {
	fmt.Println("[Type3 Hash] Start")

	reader := bytes.NewReader(inputBytes)
	ctx, err := api.ReadContext(reader, conf)
	if err != nil {
		fmt.Println("[Type3 Hash] ReadContext failed:", err)
		return
	}

	if ctx == nil || ctx.XRefTable == nil {
		fmt.Println("[Type3 Hash] No XRefTable.")
		return
	}

	if err := ctx.EnsurePageCount(); err != nil {
		fmt.Println("[Type3 Hash] EnsurePageCount failed:", err)
		return
	}

	fmt.Println("[Type3 Hash] page count:", ctx.PageCount)

	seenFonts := map[string]bool{}
	hashCounts := map[string]int{}
	hashRefs := map[string][]string{}
	totalType3 := 0
	hashErrors := 0

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

		for _, fontRef := range fontResources {
			refKey := fmt.Sprintf("%v", fontRef)
			if seenFonts[refKey] {
				continue
			}
			seenFonts[refKey] = true

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

			totalType3++
			hashValue, err := hashType3Font(ctx, fontDict)
			if err != nil {
				hashErrors++
				fmt.Printf("[Type3 Hash] ERROR ref=%s err=%v\n", refKey, err)
				continue
			}

			hashCounts[hashValue]++
			hashRefs[hashValue] = append(hashRefs[hashValue], refKey)
		}
	}

	type hashGroup struct {
		hash  string
		count int
	}

	groups := make([]hashGroup, 0, len(hashCounts))
	duplicateObjects := 0
	duplicateGroups := 0
	maxGroup := 0

	for hashValue, count := range hashCounts {
		groups = append(groups, hashGroup{hash: hashValue, count: count})
		if count > 1 {
			duplicateGroups++
			duplicateObjects += count - 1
			if count > maxGroup {
				maxGroup = count
			}
		}
	}

	sort.Slice(groups, func(i, j int) bool {
		return groups[i].count > groups[j].count
	})

	fmt.Println("[Type3 Hash] =================================")
	fmt.Println("[Type3 Hash] total Type3 objects:", totalType3)
	fmt.Println("[Type3 Hash] unique hashes:", len(hashCounts))
	fmt.Println("[Type3 Hash] duplicate groups:", duplicateGroups)
	fmt.Println("[Type3 Hash] duplicate objects removable:", duplicateObjects)
	fmt.Println("[Type3 Hash] largest duplicate group:", maxGroup)
	fmt.Println("[Type3 Hash] hash errors:", hashErrors)

	if totalType3 > 0 {
		rate := float64(duplicateObjects) / float64(totalType3) * 100
		fmt.Printf("[Type3 Hash] duplicate rate: %.1f%%\n", rate)
	}

	fmt.Println("[Type3 Hash] Top duplicate groups:")
	displayed := 0
	for _, group := range groups {
		if group.count <= 1 {
			break
		}

		shortHash := group.hash
		if len(shortHash) > 16 {
			shortHash = shortHash[:16]
		}

		fmt.Printf(
			"[Type3 Hash] count=%d hash=%s refs=%v\n",
			group.count,
			shortHash,
			hashRefs[group.hash],
		)

		displayed++
		if displayed >= 20 {
			break
		}
	}

	fmt.Println("[Type3 Hash] Finished")
}

func optimizePDF(this js.Value, args []js.Value) interface{} {
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

	inputBytes := make([]byte, length)
	copied := js.CopyBytesToGo(inputBytes, input)
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

	conf := model.NewDefaultConfiguration()
	conf.Optimize = true
	conf.OptimizeResourceDicts = true
	conf.OptimizeDuplicateContentStreams = true

	// Diagnostic only: this does not modify the PDF.
	analyzeType3Hashes(inputBytes, conf)

	reader := bytes.NewReader(inputBytes)
	var output bytes.Buffer

	err := api.Optimize(reader, &output, conf)
	if err != nil {
		return map[string]interface{}{
			"ok":    false,
			"error": fmt.Sprintf("pdfcpu Optimize failed: %v", err),
		}
	}

	outputBytes := output.Bytes()
	if len(outputBytes) <= 0 {
		return map[string]interface{}{
			"ok":    false,
			"error": "pdfcpu returned empty PDF.",
		}
	}

	jsOutput := js.Global().Get("Uint8Array").New(len(outputBytes))
	copied = js.CopyBytesToJS(jsOutput, outputBytes)
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

func main() {
	api.DisableConfigDir()

	js.Global().Set(
		"pdfcpuOptimize",
		js.FuncOf(optimizePDF),
	)

	println("pdfcpu WASM ready - Type3 deep hash scan")
	select {}
}
