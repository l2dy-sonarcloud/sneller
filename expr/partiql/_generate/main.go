// Copyright (C) 2023 Sneller, Inc.
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"crypto/md5"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
)

var stdout io.Writer

func main() {
	var (
		inpath  string
		outpath string
	)
	flag.StringVar(&inpath, "i", "", "input path")
	flag.StringVar(&outpath, "o", "", "output path")
	flag.Parse()
	if inpath == "" || outpath == "" {
		flag.Usage()
		os.Exit(1)
	}

	input, err := readEntryList(inpath)
	check(err)

	buf := bytes.NewBuffer(nil)
	stdout = buf
	writeImplementation(input)

	checksum := []byte(fmt.Sprintf("// checksum: %x\n", md5.Sum(buf.Bytes())))
	regenerate := true
	if old, err := os.ReadFile(outpath); err == nil {
		regenerate = !bytes.HasSuffix(old, checksum)
	}

	if regenerate {
		fmt.Printf("Creating %q\n", outpath)
		buf.Write(checksum)
		err := os.WriteFile(outpath, buf.Bytes(), 0644)
		check(err)
	}
}

const autogenerated = "// Code generated automatically; DO NOT EDIT"

func writeImplementation(input *Input) {
	writeln("package %s", input.packagename)
	writeln("")
	writeln(autogenerated)
	writeln("")
	if len(input.imports) > 0 {
		writeln("import (")
		for _, imp := range input.imports {
			writeln("\t%q", imp)
		}
		writeln(")")
	}
	writeln("func %s {", input.signature)
	eqfunctions := writeLookupFunctionBody(input)
	writeln("}")

	for _, length := range eqfunctions {
		writeln("")
		writeln("func equalASCIILetters%[1]d(anyCase [%[1]d]byte, upperCaseLetters [%[1]d]byte) bool {", length)
		writeln("for i := range upperCaseLetters {")
		// When we know that one byte is the ASCII upper case, then the case-insensitive compare
		// yields a value that may differ only at the 5th bit.
		writeln("if (upperCaseLetters[i] ^ anyCase[i]) & 0xdf != 0 {")
		writeln("return false")
		writeln("}")
		writeln("}") // for
		writeln("return true")
		writeln("}") // func
	}
}

func writeLookupFunctionBody(input *Input) (eqfunctions []int) {
	// 1. first split by length and determine arg lengths
	bylen := make(map[int]*[]Entry)
	minlength := 100_000
	maxlength := 0
	for _, e := range input.keywords {
		n := len(e.keyword)
		if n > maxlength {
			maxlength = n
		} else if n < minlength {
			minlength = n
		}

		if _, ok := bylen[n]; !ok {
			bylen[n] = new([]Entry)
		}

		subset := bylen[n]
		*subset = append(*subset, e)

	}

	writeln("n := len(%s)", input.argname)

	writeln("if n < %d || n > %d {", minlength, maxlength)
	writeln("\treturn %s", input.defvalue)
	writeln("}")

	// 2. for each subset of same-length keywords perform lookup
	writeln("switch n {")
	for l := minlength; l <= maxlength; l++ {
		words, ok := bylen[l]
		if !ok {
			continue
		}

		writeln("case %d:", l)
		usedEqualFn := writeSelection(*words, input.argname)
		if usedEqualFn {
			eqfunctions = append(eqfunctions, l)
		}
	}

	writeln("}")
	writeln("return %s", input.defvalue)

	return eqfunctions
}

func writeSelection(words []Entry, argname string) bool {
	l := lookup{
		length:  len(words[0].keyword),
		argname: argname,
	}

	l.freeIndices = make([]int, l.length)
	for i := 0; i < l.length; i++ {
		l.freeIndices[i] = i
	}

	l.generate(words)

	return l.usedEqualFn
}

type Entry struct {
	keyword []byte // uppercase keyword
	govalue string // Go value (copied verbatim to the output)
}

type lookup struct {
	length      int    // length of word
	freeIndices []int  // which indices of word were not tested
	argname     string // go arg name
	usedEqualFn bool   // if we need to emit comparison function for length-width strings
}

func (l *lookup) emitIf(e Entry) {
	if n := l.freeIndicesCount(); n <= 2 {
		b := new(strings.Builder)
		for _, idx := range l.freeIndices {
			if idx < 0 {
				continue
			}

			if b.Len() > 0 {
				b.WriteString(" && ")
			}

			fmt.Fprintf(b, "asciiUpper(word[%d]) == '%c'", idx, e.keyword[idx])
		}

		writeln("if %s {", b)

	} else if allUpperASCIILetters(e.keyword) {
		l.usedEqualFn = true

		b := new(strings.Builder)
		fmt.Fprintf(b, "[%d]byte{", l.length)
		for i, c := range e.keyword {
			if i > 0 {
				b.WriteString(", ")
			}

			fmt.Fprintf(b, "'%c'", c)
		}
		b.WriteString("}")

		writeln("if equalASCIILetters%[1]d([%[1]d]byte(%[2]s), %[3]s) {", l.length, l.argname, b)
	} else {
		writeln("if equalASCII(%s, []byte(%q)) {", l.argname, e.keyword)
	}

	writeln("\treturn %s", e.govalue)
	writeln("}")
}

func (l *lookup) freeIndicesCount() int {
	k := 0
	for _, idx := range l.freeIndices {
		if idx >= 0 {
			k += 1
		}
	}

	return k
}

func (l *lookup) generate(words []Entry) {
	if len(words) <= 3 {
		// there are just a few words, we don't need to do anything fancy
		for i := range words {
			l.emitIf(words[i])
		}
		return
	}

	index, subsets := splitByLetter(words, l.freeIndices)
	writeln("switch asciiUpper(%s[%d]) {", l.argname, index)

	// sort by the chosen character
	chars := make([]string, 0, len(subsets))
	for char := range subsets {
		chars = append(chars, string(char))
	}
	sort.Strings(chars)
	for _, s := range chars {
		char := s[0]
		writeln("case '%c':", char)

		l.freeIndices[index] = -1
		l.generate(*(subsets[char]))
		l.freeIndices[index] = index
	}
	writeln("}")
}

func splitByLetter(words []Entry, freeIndices []int) (index int, result map[byte]*[]Entry) {
	index = findBestSplit(words, freeIndices)
	result = make(map[byte]*[]Entry)
	for i := range words {
		char := words[i].keyword[index]
		if _, ok := result[char]; !ok {
			result[char] = new([]Entry)
		}

		*result[char] = append(*result[char], words[i])
	}

	return
}

// findBestSplit takes a list of words of the same lengths and finds
// a position of letter for which the words differ.
//
// In the best case, we may find a character unique for each word.
// Otherwise, we're looking for a character yielding the minimum
// collisions.
func findBestSplit(words []Entry, freeIndices []int) int {
	index := -1
	conflicts := len(words)

	if len(words) <= 1 {
		panic("cannot deal with an empty or 1-element list")
	}

	// search for the best match
	for i, idx := range freeIndices {
		if idx < 0 {
			continue
		}
		set := make(map[byte]struct{})
		for _, word := range words {
			c := word.keyword[idx]
			set[c] = struct{}{}
		}

		diff := len(words) - len(set)
		if diff < conflicts {
			if diff == 0 {
				return i
			}
			index = i
			conflicts = diff
		}
	}

	if index < 0 {
		panic("cannot find the split position")
	}

	return index
}

type Input struct {
	packagename string
	signature   string
	argname     string
	defvalue    string
	imports     []string

	keywords []Entry
}

func readEntryList(path string) (*Input, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	tmp := bytes.Split(buf, []byte{'\n'})

	input := new(Input)
	for _, line := range tmp {
		if len(line) == 0 || line[0] == '#' {
			continue
		}

		if key, v, ok := bytes.Cut(line, []byte{':'}); ok {
			value := strings.TrimSpace(string(v))
			switch string(key) {
			case "package":
				input.packagename = value
			case "imports":
				input.imports = append(input.imports, value)
			case "signature":
				input.signature = value
			case "argname":
				input.argname = value
			case "default":
				input.defvalue = value
			default:
				return nil, fmt.Errorf("unknown field %q", key)
			}
		} else {
			var e Entry
			key, value, ok := bytes.Cut(line, []byte{' '})
			if ok {
				e.keyword = key
				e.govalue = strings.TrimSpace(string(value))
			} else {
				e.keyword = line
				e.govalue = string(line)
			}

			input.keywords = append(input.keywords, e)
		}
	}

	return input, nil
}

func allUpperASCIILetters(s []byte) bool {
	for _, b := range s {
		if !(b >= 'A' && b <= 'Z') {
			return false
		}
	}

	return true
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func writeln(format string, args ...any) {
	fmt.Fprintf(stdout, format+"\n", args...)
}
