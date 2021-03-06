// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package prog

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
)

// String generates a very compact program description (mostly for debug output).
func (p *Prog) String() string {
	buf := new(bytes.Buffer)
	for i, c := range p.Calls {
		if i != 0 {
			fmt.Fprintf(buf, "-")
		}
		fmt.Fprintf(buf, "%v", c.Meta.Name)
	}
	return buf.String()
}

func (p *Prog) Serialize() []byte {
	if debug {
		if err := p.validate(); err != nil {
			panic("serializing invalid program")
		}
	}
	buf := new(bytes.Buffer)
	vars := make(map[Arg]int)
	varSeq := 0
	for _, c := range p.Calls {
		if len(*c.Ret.(ArgUsed).Used()) != 0 {
			fmt.Fprintf(buf, "r%v = ", varSeq)
			vars[c.Ret] = varSeq
			varSeq++
		}
		fmt.Fprintf(buf, "%v(", c.Meta.Name)
		for i, a := range c.Args {
			if IsPad(a.Type()) {
				continue
			}
			if i != 0 {
				fmt.Fprintf(buf, ", ")
			}
			serialize(a, buf, vars, &varSeq)
		}
		fmt.Fprintf(buf, ")\n")
	}
	return buf.Bytes()
}

func serialize(arg Arg, buf io.Writer, vars map[Arg]int, varSeq *int) {
	if arg == nil {
		fmt.Fprintf(buf, "nil")
		return
	}
	if used, ok := arg.(ArgUsed); ok && len(*used.Used()) != 0 {
		fmt.Fprintf(buf, "<r%v=>", *varSeq)
		vars[arg] = *varSeq
		*varSeq++
	}
	switch a := arg.(type) {
	case *ConstArg:
		fmt.Fprintf(buf, "0x%x", a.Val)
	case *PointerArg:
		if a.Res == nil && a.PagesNum == 0 {
			fmt.Fprintf(buf, "0x0")
			break
		}
		fmt.Fprintf(buf, "&%v=", serializeAddr(arg))
		serialize(a.Res, buf, vars, varSeq)
	case *DataArg:
		fmt.Fprintf(buf, "\"%v\"", hex.EncodeToString(a.Data))
	case *GroupArg:
		var delims []byte
		switch arg.Type().(type) {
		case *StructType:
			delims = []byte{'{', '}'}
		case *ArrayType:
			delims = []byte{'[', ']'}
		default:
			panic("unknown group type")
		}
		buf.Write([]byte{delims[0]})
		for i, arg1 := range a.Inner {
			if arg1 != nil && IsPad(arg1.Type()) {
				continue
			}
			if i != 0 {
				fmt.Fprintf(buf, ", ")
			}
			serialize(arg1, buf, vars, varSeq)
		}
		buf.Write([]byte{delims[1]})
	case *UnionArg:
		fmt.Fprintf(buf, "@%v=", a.OptionType.FieldName())
		serialize(a.Option, buf, vars, varSeq)
	case *ResultArg:
		if a.Res == nil {
			fmt.Fprintf(buf, "0x%x", a.Val)
			break
		}
		id, ok := vars[a.Res]
		if !ok {
			panic("no result")
		}
		fmt.Fprintf(buf, "r%v", id)
		if a.OpDiv != 0 {
			fmt.Fprintf(buf, "/%v", a.OpDiv)
		}
		if a.OpAdd != 0 {
			fmt.Fprintf(buf, "+%v", a.OpAdd)
		}
	default:
		panic("unknown arg kind")
	}
}

func (target *Target) Deserialize(data []byte) (prog *Prog, err error) {
	prog = &Prog{
		Target: target,
	}
	p := &parser{r: bufio.NewScanner(bytes.NewReader(data))}
	p.r.Buffer(nil, maxLineLen)
	vars := make(map[string]Arg)
	for p.Scan() {
		if p.EOF() || p.Char() == '#' {
			continue
		}
		name := p.Ident()
		r := ""
		if p.Char() == '=' {
			r = name
			p.Parse('=')
			name = p.Ident()

		}
		meta := target.SyscallMap[name]
		if meta == nil {
			return nil, fmt.Errorf("unknown syscall %v", name)
		}
		c := &Call{
			Meta: meta,
			Ret:  MakeReturnArg(meta.Ret),
		}
		prog.Calls = append(prog.Calls, c)
		p.Parse('(')
		for i := 0; p.Char() != ')'; i++ {
			if i >= len(meta.Args) {
				return nil, fmt.Errorf("wrong call arg count: %v, want %v", i+1, len(meta.Args))
			}
			typ := meta.Args[i]
			if IsPad(typ) {
				return nil, fmt.Errorf("padding in syscall %v arguments", name)
			}
			arg, err := target.parseArg(typ, p, vars)
			if err != nil {
				return nil, err
			}
			c.Args = append(c.Args, arg)
			if p.Char() != ')' {
				p.Parse(',')
			}
		}
		p.Parse(')')
		if !p.EOF() {
			return nil, fmt.Errorf("tailing data (line #%v)", p.l)
		}
		if len(c.Args) < len(meta.Args) {
			for i := len(c.Args); i < len(meta.Args); i++ {
				c.Args = append(c.Args, defaultArg(meta.Args[i]))
			}
		}
		if len(c.Args) != len(meta.Args) {
			return nil, fmt.Errorf("wrong call arg count: %v, want %v", len(c.Args), len(meta.Args))
		}
		if r != "" {
			vars[r] = c.Ret
		}
	}
	if err := p.Err(); err != nil {
		return nil, err
	}
	// This validation is done even in non-debug mode because deserialization
	// procedure does not catch all bugs (e.g. mismatched types).
	// And we can receive bad programs from corpus and hub.
	if err := prog.validate(); err != nil {
		return nil, err
	}
	return
}

func (target *Target) parseArg(typ Type, p *parser, vars map[string]Arg) (Arg, error) {
	r := ""
	if p.Char() == '<' {
		p.Parse('<')
		r = p.Ident()
		p.Parse('=')
		p.Parse('>')
	}
	var arg Arg
	switch p.Char() {
	case '0':
		val := p.Ident()
		v, err := strconv.ParseUint(val, 0, 64)
		if err != nil {
			return nil, fmt.Errorf("wrong arg value '%v': %v", val, err)
		}
		switch typ.(type) {
		case *ConstType, *IntType, *FlagsType, *ProcType, *LenType, *CsumType:
			arg = MakeConstArg(typ, v)
		case *ResourceType:
			arg = MakeResultArg(typ, nil, v)
		case *PtrType:
			arg = MakePointerArg(typ, 0, 0, 0, nil)
		case *VmaType:
			arg = MakePointerArg(typ, 0, 0, 0, nil)
		default:
			return nil, fmt.Errorf("bad const type %+v", typ)
		}
	case 'r':
		id := p.Ident()
		v, ok := vars[id]
		if !ok || v == nil {
			return nil, fmt.Errorf("result %v references unknown variable (vars=%+v)", id, vars)
		}
		arg = MakeResultArg(typ, v, 0)
		if p.Char() == '/' {
			p.Parse('/')
			op := p.Ident()
			v, err := strconv.ParseUint(op, 0, 64)
			if err != nil {
				return nil, fmt.Errorf("wrong result div op: '%v'", op)
			}
			arg.(*ResultArg).OpDiv = v
		}
		if p.Char() == '+' {
			p.Parse('+')
			op := p.Ident()
			v, err := strconv.ParseUint(op, 0, 64)
			if err != nil {
				return nil, fmt.Errorf("wrong result add op: '%v'", op)
			}
			arg.(*ResultArg).OpAdd = v
		}
	case '&':
		var typ1 Type
		switch t1 := typ.(type) {
		case *PtrType:
			typ1 = t1.Type
		case *VmaType:
		default:
			return nil, fmt.Errorf("& arg is not a pointer: %#v", typ)
		}
		p.Parse('&')
		page, off, size, err := parseAddr(p, true)
		if err != nil {
			return nil, err
		}
		p.Parse('=')
		inner, err := target.parseArg(typ1, p, vars)
		if err != nil {
			return nil, err
		}
		arg = MakePointerArg(typ, page, off, size, inner)
	case '(':
		// This used to parse length of VmaType and return ArgPageSize, which is now removed.
		// Leaving this for now for backwards compatibility.
		pages, _, _, err := parseAddr(p, false)
		if err != nil {
			return nil, err
		}
		arg = MakeConstArg(typ, pages*target.PageSize)
	case '"':
		p.Parse('"')
		val := ""
		if p.Char() != '"' {
			val = p.Ident()
		}
		p.Parse('"')
		data, err := hex.DecodeString(val)
		if err != nil {
			return nil, fmt.Errorf("data arg has bad value '%v'", val)
		}
		arg = MakeDataArg(typ, data)
	case '{':
		t1, ok := typ.(*StructType)
		if !ok {
			return nil, fmt.Errorf("'{' arg is not a struct: %#v", typ)
		}
		p.Parse('{')
		var inner []Arg
		for i := 0; p.Char() != '}'; i++ {
			if i >= len(t1.Fields) {
				return nil, fmt.Errorf("wrong struct arg count: %v, want %v", i+1, len(t1.Fields))
			}
			fld := t1.Fields[i]
			if IsPad(fld) {
				inner = append(inner, MakeConstArg(fld, 0))
			} else {
				arg, err := target.parseArg(fld, p, vars)
				if err != nil {
					return nil, err
				}
				inner = append(inner, arg)
				if p.Char() != '}' {
					p.Parse(',')
				}
			}
		}
		p.Parse('}')
		for len(inner) < len(t1.Fields) {
			inner = append(inner, defaultArg(t1.Fields[len(inner)]))
		}
		arg = MakeGroupArg(typ, inner)
	case '[':
		t1, ok := typ.(*ArrayType)
		if !ok {
			return nil, fmt.Errorf("'[' arg is not an array: %#v", typ)
		}
		p.Parse('[')
		var inner []Arg
		for i := 0; p.Char() != ']'; i++ {
			arg, err := target.parseArg(t1.Type, p, vars)
			if err != nil {
				return nil, err
			}
			inner = append(inner, arg)
			if p.Char() != ']' {
				p.Parse(',')
			}
		}
		p.Parse(']')
		arg = MakeGroupArg(typ, inner)
	case '@':
		t1, ok := typ.(*UnionType)
		if !ok {
			return nil, fmt.Errorf("'@' arg is not a union: %#v", typ)
		}
		p.Parse('@')
		name := p.Ident()
		p.Parse('=')
		var optType Type
		for _, t2 := range t1.Fields {
			if name == t2.FieldName() {
				optType = t2
				break
			}
		}
		if optType == nil {
			return nil, fmt.Errorf("union arg %v has unknown option: %v", typ.Name(), name)
		}
		opt, err := target.parseArg(optType, p, vars)
		if err != nil {
			return nil, err
		}
		arg = MakeUnionArg(typ, opt, optType)
	case 'n':
		p.Parse('n')
		p.Parse('i')
		p.Parse('l')
		if r != "" {
			return nil, fmt.Errorf("named nil argument")
		}
	default:
		return nil, fmt.Errorf("failed to parse argument at %v (line #%v/%v: %v)", int(p.Char()), p.l, p.i, p.s)
	}
	if r != "" {
		vars[r] = arg
	}
	return arg, nil
}

const (
	encodingAddrBase = 0x7f0000000000
	encodingPageSize = 4 << 10
	maxLineLen       = 256 << 10
)

func serializeAddr(arg Arg) string {
	var pageIndex, pagesNum uint64
	var pageOffset int
	switch a := arg.(type) {
	case *PointerArg:
		pageIndex = a.PageIndex
		pageOffset = a.PageOffset
		pagesNum = a.PagesNum
	default:
		panic("bad addr arg")
	}
	page := pageIndex * encodingPageSize
	page += encodingAddrBase
	soff := ""
	if off := pageOffset; off != 0 {
		sign := "+"
		if off < 0 {
			sign = "-"
			off = -off
			page += encodingPageSize
		}
		soff = fmt.Sprintf("%v0x%x", sign, off)
	}
	ssize := ""
	if size := pagesNum; size != 0 {
		size *= encodingPageSize
		ssize = fmt.Sprintf("/0x%x", size)
	}
	return fmt.Sprintf("(0x%x%v%v)", page, soff, ssize)
}

func parseAddr(p *parser, base bool) (uint64, int, uint64, error) {
	p.Parse('(')
	pstr := p.Ident()
	page, err := strconv.ParseUint(pstr, 0, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to parse addr page: '%v'", pstr)
	}
	if page%encodingPageSize != 0 {
		return 0, 0, 0, fmt.Errorf("address base is not page size aligned: '%v'", pstr)
	}
	if base {
		if page < encodingAddrBase {
			return 0, 0, 0, fmt.Errorf("address without base offset: '%v'", pstr)
		}
		page -= encodingAddrBase
	}
	var off int64
	if p.Char() == '+' || p.Char() == '-' {
		minus := false
		if p.Char() == '-' {
			minus = true
			p.Parse('-')
		} else {
			p.Parse('+')
		}
		ostr := p.Ident()
		off, err = strconv.ParseInt(ostr, 0, 64)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("failed to parse addr offset: '%v'", ostr)
		}
		if minus {
			page -= encodingPageSize
			off = -off
		}
	}
	var size uint64
	if p.Char() == '/' {
		p.Parse('/')
		pstr := p.Ident()
		size, err = strconv.ParseUint(pstr, 0, 64)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("failed to parse addr size: '%v'", pstr)
		}
	}
	p.Parse(')')
	page /= encodingPageSize
	size /= encodingPageSize
	return page, int(off), size, nil
}

type parser struct {
	r *bufio.Scanner
	s string
	i int
	l int
	e error
}

func (p *parser) Scan() bool {
	if p.e != nil {
		return false
	}
	if !p.r.Scan() {
		p.e = p.r.Err()
		return false
	}
	p.s = p.r.Text()
	p.i = 0
	p.l++
	return true
}

func (p *parser) Err() error {
	return p.e
}

func (p *parser) Str() string {
	return p.s
}

func (p *parser) EOF() bool {
	return p.i == len(p.s)
}

func (p *parser) Char() byte {
	if p.e != nil {
		return 0
	}
	if p.EOF() {
		p.failf("unexpected eof")
		return 0
	}
	return p.s[p.i]
}

func (p *parser) Parse(ch byte) {
	if p.e != nil {
		return
	}
	if p.EOF() {
		p.failf("want %s, got EOF", string(ch))
		return
	}
	if p.s[p.i] != ch {
		p.failf("want '%v', got '%v'", string(ch), string(p.s[p.i]))
		return
	}
	p.i++
	p.SkipWs()
}

func (p *parser) SkipWs() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

func (p *parser) Ident() string {
	i := p.i
	for p.i < len(p.s) &&
		(p.s[p.i] >= 'a' && p.s[p.i] <= 'z' ||
			p.s[p.i] >= 'A' && p.s[p.i] <= 'Z' ||
			p.s[p.i] >= '0' && p.s[p.i] <= '9' ||
			p.s[p.i] == '_' || p.s[p.i] == '$') {
		p.i++
	}
	if i == p.i {
		p.failf("failed to parse identifier at pos %v", i)
		return ""
	}
	if ch := p.s[i]; ch >= '0' && ch <= '9' {
	}
	s := p.s[i:p.i]
	p.SkipWs()
	return s
}

func (p *parser) failf(msg string, args ...interface{}) {
	p.e = fmt.Errorf("%v\nline #%v: %v", fmt.Sprintf(msg, args...), p.l, p.s)
}

// CallSet returns a set of all calls in the program.
// It does very conservative parsing and is intended to parse paste/future serialization formats.
func CallSet(data []byte) (map[string]struct{}, error) {
	calls := make(map[string]struct{})
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(nil, maxLineLen)
	for s.Scan() {
		ln := s.Bytes()
		if len(ln) == 0 || ln[0] == '#' {
			continue
		}
		bracket := bytes.IndexByte(ln, '(')
		if bracket == -1 {
			return nil, fmt.Errorf("line does not contain opening bracket")
		}
		call := ln[:bracket]
		if eq := bytes.IndexByte(call, '='); eq != -1 {
			eq++
			for eq < len(call) && call[eq] == ' ' {
				eq++
			}
			call = call[eq:]
		}
		if len(call) == 0 {
			return nil, fmt.Errorf("call name is empty")
		}
		calls[string(call)] = struct{}{}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if len(calls) == 0 {
		return nil, fmt.Errorf("program does not contain any calls")
	}
	return calls, nil
}
