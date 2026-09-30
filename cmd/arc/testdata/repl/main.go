// Command repl is a bounded, in-memory key/value REPL over core ARC sessions.
// It executes no code. Each session owns its variables and discards them on exit.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gezibash/arc/sdk/provider"
	"github.com/gezibash/arc/sdk/stdio"
)

type repl struct{}

func (repl) HandleRequest(_ context.Context, r provider.Request) (string, error) {
	return "echo: " + r.Message, nil
}
func (repl) HandleSession(_ context.Context, r provider.Request, s *provider.Stream) error {
	if s.Mode() == provider.ServerStream {
		for _, line := range []string{"SET name value\n", "GET name\n", "QUIT\n"} {
			if _, err := io.WriteString(s, line); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := io.WriteString(s, "ready\n"); err != nil {
		return err
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(s)
	scanner.Buffer(make([]byte, 512), 4096)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), " ", 3)
		reply := "invalid command"
		switch {
		case parts[0] == "QUIT":
			return nil
		case parts[0] == "SET" && len(parts) == 3:
			if len(values) >= 128 {
				if _, exists := values[parts[1]]; !exists {
					return provider.Error("too_many_variables")
				}
			}
			values[parts[1]] = parts[2]
			reply = "ok"
		case parts[0] == "GET" && len(parts) == 2:
			reply = values[parts[1]]
		}
		if _, err := fmt.Fprintln(s, reply); err != nil {
			return err
		}
	}
	return scanner.Err()
}
func main() {
	if err := stdio.Run(context.Background(), repl{}, provider.Options{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
