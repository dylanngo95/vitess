/*
Copyright 2021 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"vitess.io/vitess/go/mysql/collations/internal/charset"
	"vitess.io/vitess/go/mysql/collations/internal/uca"
)

var Output = flag.String("out", "mysqldata.go", "")
var Print8BitData = flag.Bool("full8bit", false, "")
var CleanupGlobalAllocations = flag.Bool("cleanup-allocs", false, "")

type tailoringWeights map[string][]uint16

type collationMetadata struct {
	Name    string
	Charset string
	Flags   struct {
		Binary  bool
		ASCII   bool
		Default bool
	}
	CollationImpl  string
	Number         uint
	CType          []byte
	ToLower        []byte
	ToUpper        []byte
	SortOrder      []byte
	TabToUni       []uint16
	TabFromUni     []charset.UnicodeMapping
	UCAVersion     int
	Weights        tailoringWeights
	Contractions   []uca.Contraction
	Reorder        [][4]uint16
	UpperCaseFirst bool
}

func diffMaps(orgWeights, modWeights tailoringWeights) (diff []uca.WeightPatch) {
	if len(modWeights) == 0 {
		return nil
	}

	diffMap := make(tailoringWeights)
	for key, val := range modWeights {
		if orgVal, ok := orgWeights[key]; !ok || len(orgVal) != len(val) {
			diffMap[key] = val
			continue
		}

		for i, arr := range val {
			if orgWeights[key][i] != arr {
				diffMap[key] = val
				break
			}
		}
	}

	for key, val := range diffMap {
		cp, err := strconv.ParseInt(key[2:], 16, 32)
		if err != nil {
			panic(err)
		}
		diff = append(diff, uca.WeightPatch{Codepoint: rune(cp), Patch: val})
	}

	sort.Slice(diff, func(i, j int) bool {
		return diff[i].Codepoint < diff[j].Codepoint
	})

	return
}

func dedupTable(name, coll string, val interface{}, dedup map[string]string) (string, bool) {
	raw := fmt.Sprintf("%#v", val)
	if exist, ok := dedup[raw]; ok {
		log.Printf("deduplicated %q -> %q", name+"_"+coll, exist)
		return exist, true
	}

	varname := fmt.Sprintf("%s_%s", name, coll)
	dedup[raw] = varname
	return varname, false
}

func (meta *collationMetadata) writeUcaLegacy(tables, init io.Writer, seenTables map[string]string) {
	tableWeightPatches := meta.writeWeightPatches(tables, seenTables)
	tableContractions := meta.writeContractions(tables, seenTables)

	fmt.Fprintf(init, "register(&Collation_uca_legacy{\n")
	fmt.Fprintf(init, "name: %q,\n", meta.Name)
	fmt.Fprintf(init, "id: %d,\n", meta.Number)
	fmt.Fprintf(init, "charset: charset.Charset_%s{},\n", meta.Charset)
	fmt.Fprintf(init, "weights: uca.WeightTable_uca%d,\n", meta.UCAVersion)
	if tableWeightPatches != "" {
		fmt.Fprintf(init, "tailoring: %s,\n", tableWeightPatches)
	}
	if tableContractions != "" {
		fmt.Fprintf(init, "contractions: %s,\n", tableContractions)
	}
	switch meta.UCAVersion {
	case 400:
		fmt.Fprintf(init, "maxCodepoint: 0xFFFF,\n")
	case 520:
		fmt.Fprintf(init, "maxCodepoint: 0x10FFFF,\n")
	default:
		panic("invalid UCAVersion")
	}
	fmt.Fprintf(init, "}, %v)\n", meta.Flags.Default)
}

func (meta *collationMetadata) writeWeightPatches(tables io.Writer, seenTables map[string]string) string {
	var tableWeightPatches string
	var dedup bool
	var baseWeights tailoringWeights

	switch meta.UCAVersion {
	case 400:
		baseWeights = baseWeightsUca400
	case 520:
		baseWeights = baseWeightsUca520
	case 900:
		baseWeights = baseWeightsUca900
	default:
		panic("invalid UCAVersion")
	}

	diff := diffMaps(baseWeights, meta.Weights)
	if len(diff) > 0 {
		tableWeightPatches, dedup = dedupTable("weightTailoring", meta.Name, diff, seenTables)
		if !dedup {
			fmt.Fprintf(tables, "var %s = []uca.WeightPatch{\n", tableWeightPatches)
			for _, d := range diff {
				fmt.Fprintf(tables, "{ Codepoint: %d, Patch: %#v },\n", d.Codepoint, d.Patch)
			}
			fmt.Fprintf(tables, "}\n")
		}
	}

	return tableWeightPatches
}

func (meta *collationMetadata) writeContractions(tables io.Writer, seenTables map[string]string) string {
	var tableContractions string
	var dedup bool

	if len(meta.Contractions) > 0 {
		tableContractions, dedup = dedupTable("contractions", meta.Name, meta.Contractions, seenTables)
		if !dedup {
			fmt.Fprintf(tables, "var %s = []uca.Contraction{\n", tableContractions)
			for _, ctr := range meta.Contractions {
				for i := 0; i < len(ctr.Weights)-3; i += 3 {
					if ctr.Weights[i] == 0x0 && ctr.Weights[i+1] == 0x0 && ctr.Weights[i+2] == 0x0 {
						ctr.Weights = ctr.Weights[:i]
						break
					}
				}
				fmt.Fprintf(tables, "{ Path: %#v, Weights: %#v, ", ctr.Path, ctr.Weights)
				if ctr.Contextual {
					fmt.Fprintf(tables, "Contextual: true,")
				}
				fmt.Fprintf(tables, "},\n")
			}
			fmt.Fprintf(tables, "}\n")
		}
	}
	return tableContractions
}

func (meta *collationMetadata) writeReorders(tables io.Writer, seenTables map[string]string) string {
	var tableReorder string
	var dedup bool

	if len(meta.Reorder) > 0 {
		tableReorder, dedup = dedupTable("reorder", meta.Name, meta.Reorder, seenTables)
		if !dedup {
			fmt.Fprintf(tables, "var %s = []uca.Reorder{\n", tableReorder)
			for _, r := range meta.Reorder {
				fmt.Fprintf(tables, "{FromMin: 0x%04X, FromMax: 0x%04X, ToMin: 0x%04X, ToMax: 0x%04X},\n", r[0], r[1], r[2], r[3])
			}
			fmt.Fprintf(tables, "}\n")
		}
	}
	return tableReorder
}

func (meta *collationMetadata) writeUca900(tables, init io.Writer, seenTables map[string]string) {
	if meta.UCAVersion != 900 {
		panic("unexpected UCA version for UCA900 collation")
	}

	tableWeights := "uca.WeightTable_uca900"
	switch meta.Name {
	case "utf8mb4_zh_0900_as_cs":
		// the chinese weights table is large enough that we don't apply weight patches
		// to it, we generate it as a whole
		tableWeights = "uca.WeightTable_uca900_zh"
		meta.Weights = nil

		// HACK: Chinese collations are fully reordered on their patched weights.
		// They do not need manual reordering even if they include reorder ranges
		// FIXME: Why does this collation have a reorder range that doesn't apply?
		meta.Reorder = nil

	case "utf8mb4_ja_0900_as_cs", "utf8mb4_ja_0900_as_cs_ks":
		// the japanese weights table is large enough that we don't apply weight patches
		// to it, we generate it as a whole
		tableWeights = "uca.WeightTable_uca900_ja"
		meta.Weights = nil
	}

	tableWeightPatches := meta.writeWeightPatches(tables, seenTables)
	tableContractions := meta.writeContractions(tables, seenTables)
	tableReorder := meta.writeReorders(tables, seenTables)

	fmt.Fprintf(init, "register(&Collation_utf8mb4_uca_0900{\n")
	fmt.Fprintf(init, "name: %q,\n", meta.Name)
	fmt.Fprintf(init, "id: %d,\n", meta.Number)

	var levels int
	switch {
	case strings.HasSuffix(meta.Name, "_ai_ci"):
		levels = 1
	case strings.HasSuffix(meta.Name, "_as_ci"):
		levels = 2
	case strings.HasSuffix(meta.Name, "_as_cs"):
		levels = 3
	case strings.HasSuffix(meta.Name, "_as_cs_ks"):
		levels = 4
	default:
		panic(fmt.Sprintf("unknown levelsForCompare: %q", meta.Name))
	}

	fmt.Fprintf(init, "levelsForCompare: %d,\n", levels)
	fmt.Fprintf(init, "weights: %s,\n", tableWeights)
	if tableWeightPatches != "" {
		fmt.Fprintf(init, "tailoring: %s,\n", tableWeightPatches)
	}
	if tableContractions != "" {
		fmt.Fprintf(init, "contractions: %s,\n", tableContractions)
	}
	if tableReorder != "" {
		fmt.Fprintf(init, "reorder: %s,\n", tableReorder)
	}
	if meta.UpperCaseFirst {
		fmt.Fprintf(init, "upperCaseFirst: true,\n")
	}
	fmt.Fprintf(init, "}, %v)\n", meta.Flags.Default)
}

const RowLength = 16

func printByteSlice(f io.Writer, name, coll string, a []byte, seenTables map[string]string) string {
	tableName, dedup := dedupTable(name, coll, a, seenTables)
	if !dedup {
		fmt.Fprintf(f, "var %s = []byte{", tableName)
		for idx, val := range a {
			if idx%RowLength == 0 {
				fmt.Fprintf(f, "\n")
			}
			fmt.Fprintf(f, "0x%02X, ", val)
		}
		fmt.Fprintf(f, "\n}\n")
	}
	return tableName
}

func printUnsignedSlice(f io.Writer, name, coll string, a []uint16, seenTables map[string]string) string {
	tableName, dedup := dedupTable(name, coll, a, seenTables)
	if !dedup {
		fmt.Fprintf(f, "var %s = []uint16{", tableName)
		for idx, val := range a {
			if idx%RowLength == 0 {
				fmt.Fprintf(f, "\n")
			}
			fmt.Fprintf(f, "0x%04X, ", val)
		}
		fmt.Fprintf(f, "\n}\n")
	}
	return tableName
}

func printUnicodeMappings(f io.Writer, name, coll string, mappings []charset.UnicodeMapping, seenTables map[string]string) string {
	tableName, dedup := dedupTable(name, coll, mappings, seenTables)
	if !dedup {
		fmt.Fprintf(f, "var %s = []charset.UnicodeMapping{\n", tableName)
		for _, m := range mappings {
			fmt.Fprintf(f, "{From: 0x%x, To: 0x%x, Range: %#v},\n", m.From, m.To, m.Range)
		}
		fmt.Fprintf(f, "}")
	}
	return tableName
}

func (meta *collationMetadata) write8bit(tables, init io.Writer, seenTables map[string]string) {
	var tableCtype, tableToLower, tableToUpper, tableSortOrder, tableToUnicode, tableFromUnicode string

	if *Print8BitData {
		tableCtype = printByteSlice(tables, "ctype", meta.Name, meta.CType, seenTables)
		tableToLower = printByteSlice(tables, "tolower", meta.Name, meta.ToLower, seenTables)
		tableToUpper = printByteSlice(tables, "toupper", meta.Name, meta.ToUpper, seenTables)
	}
	if meta.SortOrder != nil {
		tableSortOrder = printByteSlice(tables, "sortorder", meta.Name, meta.SortOrder, seenTables)
	}
	if meta.Charset != "latin1" {
		if meta.TabToUni != nil {
			tableToUnicode = printUnsignedSlice(tables, "tounicode", meta.Name, meta.TabToUni, seenTables)
		}
		if meta.TabFromUni != nil {
			tableFromUnicode = printUnicodeMappings(tables, "fromunicode", meta.Name, meta.TabFromUni, seenTables)
		}
	}
	fmt.Fprintf(tables, "\n")

	var collation string
	if meta.Flags.Binary {
		collation = "Collation_8bit_bin"
	} else {
		collation = "Collation_8bit_simple_ci"
	}

	fmt.Fprintf(init, "register(&%s{\n", collation)
	fmt.Fprintf(init, "id: %d,\n", meta.Number)
	fmt.Fprintf(init, "name: %q,\n", meta.Name)

	fmt.Fprintf(init, "simpletables: simpletables{\n")
	if *Print8BitData {
		fmt.Fprintf(init, "ctype: %s,\n", tableCtype)
		fmt.Fprintf(init, "tolower: %s,\n", tableToLower)
		fmt.Fprintf(init, "toupper: %s,\n", tableToUpper)
	}
	if tableSortOrder != "" {
		fmt.Fprintf(init, "sort: %s,\n", tableSortOrder)
	}
	fmt.Fprintf(init, "},\n")

	// Optimized implementation for latin1
	if meta.Charset == "latin1" {
		fmt.Fprintf(init, "charset: charset.Charset_latin1{},\n")
	} else {
		fmt.Fprintf(init, "charset: &charset.Charset_8bit{\n")
		fmt.Fprintf(init, "Name_: %q,\n", meta.Charset)
		if tableToUnicode != "" {
			fmt.Fprintf(init, "ToUnicode: %s,\n", tableToUnicode)
		}
		if tableFromUnicode != "" {
			fmt.Fprintf(init, "FromUnicode: %s,\n", tableFromUnicode)
		}
		fmt.Fprintf(init, "},\n")
	}

	fmt.Fprintf(init, "}, %v)\n", meta.Flags.Default)
}

func (meta *collationMetadata) writeUnicode(_, init io.Writer, _ map[string]string) {
	var collation string
	if meta.Flags.Binary {
		collation = "Collation_unicode_bin"
	} else {
		collation = "Collation_unicode_general_ci"
	}
	fmt.Fprintf(init, "register(&%s{\n", collation)
	fmt.Fprintf(init, "id: %d,\n", meta.Number)
	fmt.Fprintf(init, "name: %q,\n", meta.Name)
	if !meta.Flags.Binary {
		fmt.Fprintf(init, "unicase: unicaseInfo_default,\n")
	}
	fmt.Fprintf(init, "charset: charset.Charset_%s{},\n", meta.Charset)
	fmt.Fprintf(init, "}, %v)\n", meta.Flags.Default)
}

func (meta *collationMetadata) writeMultibyte(tables, init io.Writer, seenTables map[string]string) {
	var tableSortOrder string
	if meta.SortOrder != nil {
		tableSortOrder = printByteSlice(tables, "sortorder", meta.Name, meta.SortOrder, seenTables)
	}

	fmt.Fprintf(init, "register(&Collation_multibyte{\n")
	fmt.Fprintf(init, "id: %d,\n", meta.Number)
	fmt.Fprintf(init, "name: %q,\n", meta.Name)
	if tableSortOrder != "" {
		fmt.Fprintf(init, "sort: %s,\n", tableSortOrder)
	}
	fmt.Fprintf(init, "charset: charset.Charset_%s{},\n", meta.Charset)
	fmt.Fprintf(init, "}, %v)\n", meta.Flags.Default)
}

func loadMysqlMetadata() (all []*collationMetadata) {
	mysqdata, err := filepath.Glob("testdata/mysqldata/*.json")
	if err != nil {
		log.Fatal(err)
	}

	if len(mysqdata) == 0 {
		log.Fatalf("no files under 'testdata/mysqldata' (did you run colldump locally?)")
	}

	for _, path := range mysqdata {
		rf, err := os.Open(path)
		if err != nil {
			log.Fatal(err)
		}

		var meta collationMetadata
		if err := json.NewDecoder(rf).Decode(&meta); err != nil {
			log.Fatal(err)
		}
		rf.Close()

		all = append(all, &meta)
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].Number < all[j].Number
	})
	return
}

func findCollation(all []*collationMetadata, name string) *collationMetadata {
	for _, meta := range all {
		if meta.Name == name {
			return meta
		}
	}
	return nil
}

var baseWeightsUca400 tailoringWeights
var baseWeightsUca520 tailoringWeights
var baseWeightsUca900 tailoringWeights

func main() {
	var unsupportedByCharset = make(map[string][]string)
	var unsupported []*collationMetadata
	var deduplicated = make(map[string]string)
	var tables, init bytes.Buffer

	allMetadata := loadMysqlMetadata()
	baseWeightsUca400 = findCollation(allMetadata, "utf8mb4_unicode_ci").Weights
	baseWeightsUca520 = findCollation(allMetadata, "utf8mb4_unicode_520_ci").Weights
	baseWeightsUca900 = findCollation(allMetadata, "utf8mb4_0900_ai_ci").Weights

	for _, meta := range allMetadata {
		switch {
		case meta.Name == "utf8mb4_0900_bin" || meta.Name == "binary":
			// hardcoded collations; nothing to export here

		case meta.Name == "tis620_bin":
			// explicitly unsupported for now because of not accurate results

		case meta.CollationImpl == "any_uca" ||
			meta.CollationImpl == "utf16_uca" ||
			meta.CollationImpl == "utf32_uca" ||
			meta.CollationImpl == "ucs2_uca":
			meta.writeUcaLegacy(&tables, &init, deduplicated)

		case meta.CollationImpl == "uca_900":
			meta.writeUca900(&tables, &init, deduplicated)

		case meta.CollationImpl == "8bit_bin" || meta.CollationImpl == "8bit_simple_ci":
			meta.write8bit(&tables, &init, deduplicated)

		case meta.Name == "gb18030_unicode_520_ci":
			meta.writeUcaLegacy(&tables, &init, deduplicated)

		case charset.IsMultibyteByName(meta.Charset):
			meta.writeMultibyte(&tables, &init, deduplicated)

		case strings.HasSuffix(meta.Name, "_bin") && charset.IsUnicodeByName(meta.Charset):
			meta.writeUnicode(&tables, &init, deduplicated)

		case strings.HasSuffix(meta.Name, "_general_ci"):
			meta.writeUnicode(&tables, &init, deduplicated)

		default:
			unsupported = append(unsupported, meta)
			unsupportedByCharset[meta.Charset] = append(unsupportedByCharset[meta.Charset], meta.Name)
		}
	}

	var file bytes.Buffer
	fmt.Fprintf(&file, "// DO NOT MODIFY: this file is autogenerated by makemysqldata.go\n\n")
	fmt.Fprintf(&file, "package collations\n\n")
	fmt.Fprintf(&file, "import (\n")
	fmt.Fprintf(&file, "\"vitess.io/vitess/go/mysql/collations/internal/charset\"\n")
	fmt.Fprintf(&file, "\"vitess.io/vitess/go/mysql/collations/internal/uca\"\n")
	fmt.Fprintf(&file, ")\n\n")
	tables.WriteTo(&file)

	fmt.Fprintf(&file, "var collationsUnsupportedByName = map[string]ID{\n")
	for _, meta := range unsupported {
		fmt.Fprintf(&file, "%q: %d,\n", meta.Name, meta.Number)
	}
	fmt.Fprintf(&file, "}\n\n")

	fmt.Fprintf(&file, "func init() {\n")
	init.WriteTo(&file)

	if *CleanupGlobalAllocations {
		var cleanup []string
		for _, table := range deduplicated {
			cleanup = append(cleanup, table)
		}
		sort.Strings(cleanup)
		for _, table := range cleanup {
			fmt.Fprintf(&file, "%s = nil\n", table)
		}
	}

	fmt.Fprintf(&file, "}\n")

	formatted, err := format.Source(file.Bytes())
	if err != nil {
		fmt.Fprintf(os.Stderr, "source:\n%s\n", file.String())
		log.Fatalf("failed to format generated code: %v", err)
	}

	err = os.WriteFile(*Output, formatted, 0644)
	if err != nil {
		log.Fatalf("failed to generate %q: %v", *Output, err)
	}

	var unhandledCount int
	for impl, collations := range unsupportedByCharset {
		log.Printf("unhandled implementation %q: %s", impl, strings.Join(collations, ", "))
		unhandledCount += len(collations)
	}

	log.Printf("written %q - %d bytes, %d/%d collations (%.2f%% handled)",
		*Output, len(formatted),
		len(allMetadata)-unhandledCount, len(allMetadata),
		float64(len(allMetadata)-unhandledCount)/float64(len(allMetadata))*100.0,
	)
}
