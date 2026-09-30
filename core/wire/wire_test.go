package wire_test

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gezibash/arc/core/wire"
)

func TestOversizedLinesAreDrainedIncludingAtEOF(t *testing.T) {
	for _, suffix := range []string{"", "\nnext\n"} {
		reader := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 1000)+suffix), 32)
		line, err := wire.ReadLine(reader, 20)
		if !errors.Is(err, wire.ErrLineTooLong) || len(line) != 0 {
			t.Fatalf("line=%q err=%v", line, err)
		}
		line, err = wire.ReadLine(reader, 20)
		if suffix == "" {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("after oversized EOF=%v", err)
			}
		} else if err != nil || string(line) != "next" {
			t.Fatalf("next line=%q err=%v", line, err)
		}
	}
}
