package profiletest

import "testing"

// TestNextFieldRejectsAHugeDeclaredLength covers N-16: the length is checked
// before it is converted to int. On a 32-bit build int(size) wrapped a
// 0xFFFFFFFF length into a small positive number and the field was accepted.
func TestNextFieldRejectsAHugeDeclaredLength(t *testing.T) {
	raw := []byte{0x12} // field 2, wire type 2
	raw = append(raw, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01)
	raw = append(raw, 0x00)
	if _, _, _, _, ok := NextField(raw, 0); ok {
		t.Fatal("NextField accepted a length larger than the buffer")
	}
}
