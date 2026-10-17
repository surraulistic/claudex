package codex_test

import (
	"bytes"
	"testing"

	"github.com/surraulistic/claudex/internal/codex"
)

func FuzzReadFrame(f *testing.F) {
	var b bytes.Buffer
	codex.WriteFrame(&b, 0x1, []byte(`{"id":1,"result":{}}`), true)
	f.Add(b.Bytes())
	f.Add([]byte{0x81, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	f.Add([]byte{0x01, 0x7E, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		for i := 0; i < 8; i++ {
			_, _, p, err := codex.ReadFrame(r)
			if err != nil {
				return
			}
			if len(p) > len(data) {
				t.Fatalf("кадр длиннее входа: %d > %d", len(p), len(data))
			}
		}
	})
}
