package main

import (
	"encoding/binary"
	"testing"
)

// helloFCB is samples/hello-world/hello.fc compiled by fcc, byte for byte.
//
//	workflow: HelloWorld
//	step greeting:
//	    emit
//	        value = "hello, world"
//	end
//	step saved:
//	    store set
//	        key = "greeting"
//	        value = greeting
//	end
//
// Held as a literal so the decoder is tested against the real compiler's output
// without needing fcc present.
var helloFCB = []byte{
	// header: "FCB1", version 1, reserved 0, 2 instructions, 20-byte arg blob
	'F', 'C', 'B', '1',
	0x01, 0x00, 0x00, 0x00,
	0x02, 0x00, 0x00, 0x00,
	0x14, 0x00, 0x00, 0x00,
	// EMIT  offset 0  length 12
	0x01, 0x00, 0x00, 0x00, 0x00, 0x0c, 0x00, 0x00, 0x00,
	// STORE offset 12 length 8
	0x07, 0x0c, 0x00, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00,
	// arg blob
	'h', 'e', 'l', 'l', 'o', ',', ' ', 'w', 'o', 'r', 'l', 'd',
	'g', 'r', 'e', 'e', 't', 'i', 'n', 'g',
}

func TestDisassembleHelloWorld(t *testing.T) {
	bc, err := disassemble(helloFCB)
	if err != nil {
		t.Fatalf("disassemble: %v", err)
	}

	if bc.Version != 1 {
		t.Errorf("Version = %d, want 1", bc.Version)
	}
	if bc.InstructionCount != 2 {
		t.Errorf("InstructionCount = %d, want 2", bc.InstructionCount)
	}
	if bc.ArgBlobSize != 20 {
		t.Errorf("ArgBlobSize = %d, want 20", bc.ArgBlobSize)
	}
	if bc.SizeBytes != 54 {
		t.Errorf("SizeBytes = %d, want 54", bc.SizeBytes)
	}
	if len(bc.Instructions) != 2 {
		t.Fatalf("len(Instructions) = %d, want 2", len(bc.Instructions))
	}

	if got, want := bc.Instructions[0].Opcode, "EMIT"; got != want {
		t.Errorf("instruction 0 opcode = %q, want %q", got, want)
	}
	if got, want := bc.Instructions[0].Arg, `"hello, world"`; got != want {
		t.Errorf("instruction 0 arg = %q, want %q", got, want)
	}
	if got, want := bc.Instructions[1].Opcode, "STORE"; got != want {
		t.Errorf("instruction 1 opcode = %q, want %q", got, want)
	}
	if got, want := bc.Instructions[1].Arg, `"greeting"`; got != want {
		t.Errorf("instruction 1 arg = %q, want %q", got, want)
	}
}

func TestDisassembleJumpTargets(t *testing.T) {
	// A ROUTE whose 4-byte argument is the jump target 7. Rendering that blob
	// as text would produce a control character, so it must be decoded as a
	// target instead.
	arg := make([]byte, 4)
	binary.LittleEndian.PutUint32(arg, 7)

	img := buildFCB(t, []rawInstr{{op: 0x05, off: 0, length: 4}}, arg)

	bc, err := disassemble(img)
	if err != nil {
		t.Fatalf("disassemble: %v", err)
	}
	ins := bc.Instructions[0]
	if ins.Opcode != "ROUTE" {
		t.Errorf("opcode = %q, want ROUTE", ins.Opcode)
	}
	if ins.Target == nil || *ins.Target != 7 {
		t.Fatalf("Target = %v, want 7", ins.Target)
	}
	if ins.Arg != "-> instruction 7" {
		t.Errorf("Arg = %q, want %q", ins.Arg, "-> instruction 7")
	}
}

func TestDisassembleRejectsMalformed(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"short header", []byte{'F', 'C', 'B', '1', 0x01}},
		{"bad magic", append([]byte("NOPE"), helloFCB[4:]...)},
		{"truncated body", helloFCB[:len(helloFCB)-4]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := disassemble(tt.data); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestDisassembleUnsupportedVersion(t *testing.T) {
	img := append([]byte(nil), helloFCB...)
	binary.LittleEndian.PutUint16(img[4:6], 99)
	if _, err := disassemble(img); err == nil {
		t.Fatal("expected an error for version 99")
	}
}

// TestDisassembleOutOfBoundsArg covers a header that passes the size check but
// whose instruction points outside the blob — the case that would panic if the
// per-instruction bounds check were dropped.
func TestDisassembleOutOfBoundsArg(t *testing.T) {
	img := buildFCB(t, []rawInstr{{op: 0x01, off: 4, length: 100}}, []byte("abcd"))

	bc, err := disassemble(img)
	if err != nil {
		t.Fatalf("disassemble: %v", err)
	}
	if got := bc.Instructions[0].Arg; got == "" || got[0] != '<' {
		t.Errorf("Arg = %q, want an out-of-bounds note", got)
	}
}

func TestDisassembleUnknownOpcode(t *testing.T) {
	img := buildFCB(t, []rawInstr{{op: 0x42, off: 0, length: 0}}, nil)

	bc, err := disassemble(img)
	if err != nil {
		t.Fatalf("disassemble: %v", err)
	}
	if bc.Instructions[0].Opcode != "UNKNOWN" {
		t.Errorf("Opcode = %q, want UNKNOWN", bc.Instructions[0].Opcode)
	}
	if bc.Instructions[0].OpcodeHex != "0x42" {
		t.Errorf("OpcodeHex = %q, want 0x42", bc.Instructions[0].OpcodeHex)
	}
}

func TestQuoteReadableKeepsInterpolation(t *testing.T) {
	// Template syntax is the most common thing in a real arg blob; escaping it
	// the way strconv.Quote would makes the bytecode pane unreadable.
	got := quoteReadable(`order.{{order.id}}`)
	if want := `"order.{{order.id}}"`; got != want {
		t.Errorf("quoteReadable = %q, want %q", got, want)
	}
}

type rawInstr struct {
	op     uint8
	off    uint32
	length uint32
}

// buildFCB assembles a syntactically valid .fcb image for tests.
func buildFCB(t *testing.T, instrs []rawInstr, blob []byte) []byte {
	t.Helper()

	out := make([]byte, 0, fcbHeaderSize+len(instrs)*fcbInstrSize+len(blob))
	out = append(out, 'F', 'C', 'B', '1')
	out = binary.LittleEndian.AppendUint16(out, fcbVersion)
	out = binary.LittleEndian.AppendUint16(out, 0)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(instrs)))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(blob)))

	for _, in := range instrs {
		out = append(out, in.op)
		out = binary.LittleEndian.AppendUint32(out, in.off)
		out = binary.LittleEndian.AppendUint32(out, in.length)
	}
	return append(out, blob...)
}
