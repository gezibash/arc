package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	execadapter "github.com/gezibash/arc/adapters/exec"
	"github.com/gezibash/arc/application/iface"
	"github.com/gezibash/arc/core/session"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func terminalRequest(input io.Reader, body string) (string, error) {
	file, ok := input.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return "", fmt.Errorf("--tty requires a terminal")
	}
	var request map[string]any
	if err := json.Unmarshal([]byte(body), &request); err != nil || request == nil {
		return "", fmt.Errorf("terminal request must be a JSON object")
	}
	cols, rows, err := term.GetSize(int(file.Fd()))
	if err != nil {
		return "", err
	}
	request["pty"] = true
	request["rows"] = rows
	request["cols"] = cols
	data, err := json.Marshal(request)
	return string(data), err
}
func execSession(command *cobra.Command, stream *session.Stream, tty bool) error {
	input := command.InOrStdin()
	if tty {
		held, err := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer held.Close()
		input = held
	}
	var mu sync.Mutex
	encoder := json.NewEncoder(stream)
	send := func(r execadapter.Record) error { mu.Lock(); defer mu.Unlock(); return encoder.Encode(r) }
	if tty {
		file := input.(*os.File)
		old, err := term.MakeRaw(int(file.Fd()))
		if err != nil {
			return err
		}
		defer term.Restore(int(file.Fd()), old)
		resize := make(chan os.Signal, 1)
		signal.Notify(resize, syscall.SIGWINCH)
		defer signal.Stop(resize)
		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case <-resize:
					cols, rows, err := term.GetSize(int(file.Fd()))
					if err == nil {
						if err = send(execadapter.Record{Type: "resize", Rows: uint16(rows), Cols: uint16(cols)}); err != nil {
							return
						}
					}
				case <-stream.Context().Done():
					return
				case <-done:
					return
				}
			}
		}()
	}
	if stream.Mode() == session.Duplex {
		// Do not close the terminal before its saved attributes have been restored.
		{
			stop := context.AfterFunc(stream.Context(), func() {
				if closer, ok := input.(io.Closer); ok {
					closer.Close()
				}
			})
			defer stop()
		}
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := input.Read(buf)
				if n > 0 {
					if e := send(execadapter.Record{Type: "stdin", Data: buf[:n]}); e != nil {
						return
					}
				}
				if err == io.EOF {
					_ = stream.CloseWrite()
					return
				}
				if err != nil {
					_ = stream.Close()
					return
				}
			}
		}()
	}
	reader := execadapter.NewReader(stream)
	exit := -1
	for {
		record, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if exit >= 0 {
			return fmt.Errorf("Exec output after exit status")
		}
		switch record.Type {
		case "stdout":
			_, err = command.OutOrStdout().Write(record.Data)
		case "stderr":
			_, err = command.ErrOrStderr().Write(record.Data)
		case "exit":
			if record.Exit < 0 || record.Exit > 255 {
				return fmt.Errorf("invalid Exec exit status")
			}
			exit = record.Exit
		default:
			return fmt.Errorf("unknown Exec record %q", record.Type)
		}
		if err != nil {
			return err
		}
	}
	if err := stream.Wait(); err != nil {
		return err
	}
	if exit < 0 {
		return fmt.Errorf("Exec session ended without exit status")
	}
	if exit != 0 {
		return iface.ExitError{Code: exit}
	}
	return nil
}
