package engine

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The .fcb container, as written by flowcode's src/bytecode.c:
//
//	header  16 bytes  "FCB1" | u16 version | u16 reserved | u32 instrCount | u32 argSize
//	code    instrCount * 9    packed { u8 opcode, u32 argOffset, u32 argLength }
//	args    argSize           opaque blob; each instruction slices it
//
// All integers little-endian. There are no per-argument length prefixes in the
// blob — an argument is exactly the bytes its instruction points at.
const (
	fcbMagic      = "FCB1"
	fcbVersion    = 1
	fcbHeaderSize = 16
	fcbInstrSize  = 9
)

// Opcode values from include/flowcode.h (fc_opcode_t).
var opcodeNames = map[uint8]string{
	0x01: "EMIT",
	0x02: "AWAIT",
	0x03: "CALL",
	0x04: "TRANSFORM",
	0x05: "ROUTE",
	0x06: "LOOP",
	0x07: "STORE",
}

// ROUTE and LOOP don't carry a payload — their argument is a 4-byte absolute
// jump target. Rendering those as a string would show mojibake, so they get
// their own presentation.
func isJumpOpcode(op uint8) bool { return op == 0x05 || op == 0x06 }

// Instruction is one decoded bytecode instruction, shaped for the UI.
type Instruction struct {
	Index     int    `json:"index"`
	Opcode    string `json:"opcode"`
	OpcodeHex string `json:"opcodeHex"`
	ArgOffset uint32 `json:"argOffset"`
	ArgLength uint32 `json:"argLength"`
	// Arg is the human-readable rendering: a quoted string, a jump target, or
	// hex when the bytes aren't printable text.
	Arg string `json:"arg"`
	// Target is set for ROUTE/LOOP so the UI can link to the instruction.
	Target *uint32 `json:"target,omitempty"`
}

// Bytecode is the decoded program.
type Bytecode struct {
	Version          int           `json:"version"`
	InstructionCount int           `json:"instructionCount"`
	ArgBlobSize      int           `json:"argBlobSize"`
	SizeBytes        int           `json:"sizeBytes"`
	Instructions     []Instruction `json:"instructions"`
}

// disassemble decodes a .fcb image. We produced this file ourselves moments
// ago, but it is still parsed defensively — every offset is bounds-checked
// before it indexes the blob, so a malformed or truncated image yields an
// error rather than a panic that takes the server down with it.
func disassemble(data []byte) (*Bytecode, error) {
	if len(data) < fcbHeaderSize {
		return nil, fmt.Errorf("truncated header: %d bytes, need %d", len(data), fcbHeaderSize)
	}
	if string(data[0:4]) != fcbMagic {
		return nil, fmt.Errorf("bad magic %q, expected %q", string(data[0:4]), fcbMagic)
	}

	version := binary.LittleEndian.Uint16(data[4:6])
	if version != fcbVersion {
		return nil, fmt.Errorf("unsupported bytecode version %d", version)
	}
	instrCount := binary.LittleEndian.Uint32(data[8:12])
	argSize := binary.LittleEndian.Uint32(data[12:16])

	// Reject sizes that would overflow the buffer before allocating for them.
	need := int64(fcbHeaderSize) + int64(instrCount)*fcbInstrSize + int64(argSize)
	if need > int64(len(data)) {
		return nil, fmt.Errorf("truncated image: header declares %d bytes, file has %d", need, len(data))
	}

	codeStart := fcbHeaderSize
	argStart := codeStart + int(instrCount)*fcbInstrSize
	argBlob := data[argStart : argStart+int(argSize)]

	bc := &Bytecode{
		Version:          int(version),
		InstructionCount: int(instrCount),
		ArgBlobSize:      int(argSize),
		SizeBytes:        len(data),
		Instructions:     make([]Instruction, 0, instrCount),
	}

	for i := 0; i < int(instrCount); i++ {
		off := codeStart + i*fcbInstrSize
		raw := data[off : off+fcbInstrSize]

		op := raw[0]
		argOffset := binary.LittleEndian.Uint32(raw[1:5])
		argLength := binary.LittleEndian.Uint32(raw[5:9])

		name, known := opcodeNames[op]
		if !known {
			name = "UNKNOWN"
		}

		ins := Instruction{
			Index:     i,
			Opcode:    name,
			OpcodeHex: fmt.Sprintf("0x%02x", op),
			ArgOffset: argOffset,
			ArgLength: argLength,
		}

		// Mirrors fc_program_validate's bounds checks, phrased for a reader.
		if uint64(argOffset)+uint64(argLength) > uint64(argSize) {
			ins.Arg = fmt.Sprintf("<out of bounds: offset %d + length %d exceeds blob size %d>",
				argOffset, argLength, argSize)
			bc.Instructions = append(bc.Instructions, ins)
			continue
		}

		payload := argBlob[argOffset : argOffset+argLength]

		switch {
		case isJumpOpcode(op) && argLength == 4:
			target := binary.LittleEndian.Uint32(payload)
			ins.Target = &target
			ins.Arg = fmt.Sprintf("-> instruction %d", target)
		case argLength == 0:
			ins.Arg = ""
		case isPrintable(payload):
			ins.Arg = quoteReadable(string(payload))
		default:
			ins.Arg = "0x" + hex.EncodeToString(payload)
		}

		bc.Instructions = append(bc.Instructions, ins)
	}

	return bc, nil
}

// isPrintable reports whether the payload reads as text worth showing as text.
// Control characters other than tab/newline mean it isn't.
func isPrintable(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// quoteReadable wraps the payload in double quotes while keeping the inner text
// readable — strconv.Quote would escape every non-ASCII rune, which turns an
// interpolated template like "order.{{order.id}}" into noise for no benefit.
func quoteReadable(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
