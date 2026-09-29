package limitio_test

import (
	"io"
	"strings"
	"testing"

	"github.com/gezibash/arc/internal/limitio"
)

func TestCopyDrainsInputButRetainsOnlyThePrefix(t *testing.T) {
	buffer := &limitio.Buffer{Max: 5}
	// Hide WriterTo so io.Copy also exercises a destination's ReaderFrom path.
	input := struct{ io.Reader }{strings.NewReader("abcdefghij")}
	n, err := io.Copy(buffer, input)
	if err != nil || n != 10 || string(buffer.Bytes()) != "abcde" || !buffer.Truncated {
		t.Fatalf("copied=%d retained=%q truncated=%v err=%v", n, buffer.Bytes(), buffer.Truncated, err)
	}
	if n, err := buffer.Write([]byte("more")); err != nil || n != 4 || buffer.Len() != 5 {
		t.Fatalf("a full buffer stopped draining: n=%d size=%d err=%v", n, buffer.Len(), err)
	}
}
