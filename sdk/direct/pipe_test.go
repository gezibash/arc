package direct

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"
	"time"
)

// pipePair dials from offerer to answerer, with Accept in the open
// function, as a session would carry the offer and the answer. serve gets
// the pipe of the answerer after its data channel opens.
func pipePair(t *testing.T, offerer DialConfig, answerer AcceptConfig, serve func(*Pipe)) (*Pipe, error) {
	t.Helper()
	ctx := t.Context()
	return Dial(ctx, offerer, func(ctx context.Context, o Offer) (string, error) {
		p, answer, err := Accept(ctx, answerer, o)
		if err != nil {
			return "", err
		}
		go func() {
			if p.Wait(ctx) == nil {
				serve(p)
			}
		}()
		return answer, nil
	})
}

// TestPipe sends 8 MiB on a pipe between two ends on this machine. The
// reader gets each byte and then io.EOF, and the writer learns that the
// reader has the last byte.
func TestPipe(t *testing.T) {
	data := make([]byte, 8<<20)
	_, _ = rand.Read(data)
	local := Options{Loopback: true}
	acked := make(chan error, 1)
	pipe, err := pipePair(t, DialConfig{Policy: DirectOnly, Options: local}, AcceptConfig{Policy: Auto, Options: local}, func(p *Pipe) {
		defer p.Close()
		if _, err := p.Write(data); err != nil {
			acked <- err
			return
		}
		acked <- p.CloseWrite(t.Context())
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	if path := pipe.Path(); path != "direct (host to host)" {
		t.Errorf("path %q, want direct (host to host)", path)
	}
	got, err := io.ReadAll(pipe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes, not the %d bytes that were written", len(got), len(data))
	}
	select {
	case err := <-acked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the writer got no ok")
	}
}

// TestPipeClosedEarly closes the writer after part of the bytes. The reader
// gets ErrPipeClosed, not io.EOF, so it cannot take a short body as all of
// it.
func TestPipeClosedEarly(t *testing.T) {
	local := Options{Loopback: true}
	pipe, err := pipePair(t, DialConfig{Policy: DirectOnly, Options: local}, AcceptConfig{Options: local}, func(p *Pipe) {
		_, _ = p.Write(make([]byte, 64<<10))
		time.Sleep(200 * time.Millisecond)
		_ = p.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	got, err := io.ReadAll(pipe)
	if !errors.Is(err, ErrPipeClosed) {
		t.Fatalf("read %d bytes and %v, want ErrPipeClosed", len(got), err)
	}
}

// TestPipeAnswererRelayOnly gives the answerer the policy turn: its answer
// holds only an address on the TURN server, and the bytes go through the
// relay. Both ends are behind symmetric NATs.
func TestPipeAnswererRelayOnly(t *testing.T) {
	a, b, relay := natPair(t)
	const stun = "stun:1.2.3.4:3478"
	turn := func(context.Context) (Credential, error) {
		c, err := relay.Credential("0000000000000000000000000000000000000000000000000000000000000003", "1.2.3.4")
		return Credential{URLs: c.URLs, Username: c.Username, Password: c.Password, QuotaLeft: 1 << 30}, err
	}
	pipe, err := pipePair(t, DialConfig{Policy: DirectOnly, Options: Options{STUN: stun, Net: a}},
		AcceptConfig{Policy: RelayOnly, Options: Options{STUN: stun, Net: b}, TURN: turn}, func(p *Pipe) {
			defer p.Close()
			if _, err := p.Write(make([]byte, 256<<10)); err == nil {
				_ = p.CloseWrite(t.Context())
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	if path := pipe.Path(); path != "turn (relay)" {
		t.Errorf("path %q, want turn (relay)", path)
	}
	if n, err := io.Copy(io.Discard, pipe); err != nil || n != 256<<10 {
		t.Fatalf("read %d bytes, %v", n, err)
	}
}

func TestParsePolicy(t *testing.T) {
	for value, want := range map[string]Policy{"": Auto, "auto": Auto, "direct": DirectOnly, "turn": RelayOnly, "session": Off} {
		if got, err := ParsePolicy(value); err != nil || got != want {
			t.Errorf("ParsePolicy(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
	if _, err := ParsePolicy("relay"); err == nil {
		t.Error("ParsePolicy took relay")
	}
}
