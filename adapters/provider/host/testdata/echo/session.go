package main

import (
	"bufio"
	"context"
	"io"
	"strings"

	"github.com/gezibash/arc/sdk/provider"
)

func (e *echo) SetSessionCaller(c provider.SessionCaller) { e.sessions = c }
func (e *echo) HandleSession(ctx context.Context, r provider.Request, s *provider.Stream) error {
	if r.Message == "deny" {
		return provider.Error("unauthorized")
	}
	if address, ok := strings.CutPrefix(r.Message, "session "); ok {
		upstream, err := e.sessions.OpenSession(ctx, address, "", s.Mode())
		if err != nil {
			return err
		}
		defer upstream.Close()
		if s.Mode() == provider.Duplex {
			go func() { _, _ = io.Copy(upstream, s); _ = upstream.CloseWrite() }()
		}
		_, err = io.Copy(s, upstream)
		return err
	}
	if s.Mode() == provider.ServerStream {
		for _, part := range []string{"first\n", "second\n"} {
			if _, err := io.WriteString(s, part); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := io.WriteString(s, "ready\n"); err != nil {
		return err
	}
	value := "empty"
	scanner := bufio.NewScanner(s)
	for scanner.Scan() {
		line := scanner.Text()
		if v, ok := strings.CutPrefix(line, "SET "); ok {
			value = v
		}
		if _, err := io.WriteString(s, value+"\n"); err != nil {
			return err
		}
	}
	return scanner.Err()
}
