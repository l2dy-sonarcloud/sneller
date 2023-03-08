// Copyright (C) 2022 Sneller, Inc.
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
	"bufio"
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"

	"golang.org/x/exp/slices"
)

type opcode struct {
	name   string
	offset int64

	// if opcode is a different implementation of an existing opcode, base points to that opcode
	base string
}

type oprename struct {
	from, to string
}

type AsmParser struct {
	paths   []string            // stack of paths
	seen    map[string]struct{} // already seen paths
	offset  int64               // current offset in constant table
	opcodes []opcode            // parsed opcodes
}

func (a *AsmParser) parseAll() ([]opcode, error) {
	for len(a.paths) > 0 {
		n := len(a.paths)
		path := a.paths[n-1]
		a.paths = a.paths[:n-1]

		err := a.parse(path)
		if err != nil {
			return nil, err
		}
	}

	return a.opcodes, nil
}

var systemincludes = []string{"go_asm.h", "funcdata.h", "textflag.h"}

func (a *AsmParser) addPath(path string) {
	if a.seen == nil {
		a.seen = make(map[string]struct{})
		for _, p := range systemincludes {
			a.seen[p] = struct{}{}
		}
	}

	if _, ok := a.seen[path]; ok {
		return
	}

	a.paths = append(a.paths, path)
	a.seen[path] = struct{}{}
}

var reopcode = regexp.MustCompile(`^TEXT bc(?P<op>.*)\(SB\)`)

func (a *AsmParser) parse(path string) error {
	f, err := os.Open(path)
	checkErr(err)
	defer f.Close()

	rd := bufio.NewReader(f)

	scanner := bufio.NewScanner(rd)
	scanner.Split(bufio.ScanLines)
	for scanner.Scan() {
		line := scanner.Text()
		if include, ok := strings.CutPrefix(line, "#include "); ok {
			n := len(include)
			if n > 2 && include[0] == '"' && include[n-1] == '"' {
				a.addPath(include[1 : n-1])
			} else {
				return fmt.Errorf("malformed include: %s", line)
			}
			continue
		}

		if !strings.HasPrefix(line, "TEXT bc") {
			continue
		}

		if v := reopcode.FindStringSubmatch(line); len(v) > 0 {
			a.opcodes = append(a.opcodes, opcode{name: v[1], offset: a.offset})
			a.offset += 8
		}
	}

	return scanner.Err()
}

func parseAsmFile(path string) ([]opcode, []oprename, error) {
	var p AsmParser
	p.addPath(path)

	ops, err := p.parseAll()
	if err != nil {
		return nil, nil, err
	}

	lookup := make(map[string]*opcode)
	for i := range ops {
		lookup[ops[i].name] = &ops[i]
	}

	buildrename := func(suffix string) ([]oprename, error) {
		var rename []oprename
		for i := range ops {
			base, ok := stripSuffix(ops[i].name, suffix)
			if !ok {
				continue
			}

			if _, ok := lookup[base]; !ok {
				return nil, fmt.Errorf("cound not find base opcode for %q", ops[i].name)
			}

			ops[i].base = base

			rename = append(rename, oprename{from: ops[i].base, to: ops[i].name})
		}

		return rename, nil
	}

	rename, err := buildrename("_v2")

	return ops, rename, err
}

func generateGoFile(path string, ops []opcode, renameLevel2 []oprename) {
	buf := bytes.NewBuffer(nil)
	generateGo(buf, ops, renameLevel2)

	checksum := []byte(fmt.Sprintf("// checksum: %x\n", md5.Sum(buf.Bytes())))
	regenerate := true
	old, err := os.ReadFile(path)
	if err == nil {
		regenerate = !bytes.HasSuffix(old, checksum)
	}

	if regenerate {
		fmt.Printf("Creating %q\n", path)

		f, err := os.Create(path)
		checkErr(err)
		defer f.Close()
		_, err = f.Write(buf.Bytes())
		checkErr(err)
		_, err = f.Write(checksum)
		checkErr(err)
	}
}

func generateAsmFile(path string, ops []opcode) {
	buf := bytes.NewBuffer(nil)
	generateAsm(buf, ops)

	old, _ := os.ReadFile(path)
	if !slices.Equal(old, buf.Bytes()) {
		fmt.Printf("Creating %q\n", path)
		err := os.WriteFile(path, buf.Bytes(), 0644)
		checkErr(err)
	}
}

const autogenerated = "// Code generated automatically; DO NOT EDIT"

func generateGo(f io.Writer, ops []opcode, renameLevel2 []oprename) {
	write := func(s string, args ...any) {
		fmt.Fprintf(f, s, args...)
		fmt.Fprintf(f, "\n")
	}

	write("package vm")
	write("")
	write(autogenerated)
	write("")
	write("const (")

	for i := range ops {
		if strings.Contains(ops[i].name, "_") {
			write(" //lint:ignore ST1003 opcode naming convention")
		}

		write("\top%s bcop = %d", ops[i].name, i)
	}

	write("\t%s = %d", "_maxbcop", len(ops))
	write(")")

	write("")
	write("type opreplace struct {from, to bcop}")
	write("var patchAVX512Level2 []opreplace = []opreplace{")
	for i := range renameLevel2 {
		write("\t{from: op%s, to: op%s},", renameLevel2[i].from, renameLevel2[i].to)
	}
	write("}")
}

func generateAsm(f io.Writer, ops []opcode) {
	write := func(s string, args ...any) {
		fmt.Fprintf(f, s, args...)
		fmt.Fprintf(f, "\n")
	}

	write(`#include "textflag.h"`)
	write("")
	write(autogenerated)
	write("")

	const data = "opaddrs"
	const trap = "trap"

	for i := range ops {
		write("DATA %s+0x%03x(SB)/8, $bc%s(SB)", data, ops[i].offset, ops[i].name)
	}

	offset := ops[len(ops)-1].offset
	n := nextPower(len(ops))
	for i := len(ops); i < n; i++ {
		offset += 8
		write("DATA %s+0x%03x(SB)/8, $bc%s(SB)", data, offset, trap)
	}
	offset += 8

	write("GLOBL %s(SB), RODATA|NOPTR, $0x%04x", data, offset)
}

func main() {
	ops, rename, err := parseAsmFile("evalbc_amd64.s")
	checkErr(err)

	generateGoFile("ops_gen.go", ops, rename)
	generateAsmFile("ops_gen_amd64.s", ops)
}

func checkErr(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func nextPower(x int) int {
	n := 1
	for n < x {
		n *= 2
	}

	return n
}

func stripSuffix(s, suff string) (string, bool) {
	s1 := strings.TrimSuffix(s, suff)
	return s1, len(s1) != len(s)
}
