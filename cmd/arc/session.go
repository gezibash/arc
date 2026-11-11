package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gezibash/arc/core/session"
	httpadapter "github.com/gezibash/arc/sdk/httpadapter"
	"github.com/spf13/cobra"
)

func sessionCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "session <address> [initial request]",
		Short: "Open an installed capability's live interaction",
		Long: "Open a core ARC session. Duplex sessions read ongoing input from stdin\n" +
			"and write service output as it arrives. Input EOF allows final output.\n" +
			"The service must declare the selected mode; a disconnect ends the session.",
		Args: cobra.RangeArgs(1, 2),
		RunE: runSession,
	}
	command.Flags().Bool("http", false, "decode streaming HTTP response bodies")
	command.Flags().String("path", "", "path of an HTTP response body: auto, direct, turn or session (default: arc path)")
	command.Flags().Bool("websocket", false, "send input lines as WebSocket text messages")
	command.Flags().Bool("exec", false, "decode Exec output records and encode stdin")
	command.Flags().Bool("tty", false, "use an Exec PTY and a raw local terminal; Enter ~ . detaches")
	command.Flags().String("mode", string(session.Duplex), "request_reply, server_stream, or duplex")
	command.Flags().Duration("timeout", 2*time.Minute, "session lifetime, at most 30 minutes")
	return command
}

// closeOnEnd closes owned input when the session ends, so a blocked read
// returns. The returned function stops the close.
func closeOnEnd(stream *session.Stream, input io.Reader) func() bool {
	return context.AfterFunc(stream.Context(), func() {
		if closer, ok := input.(io.Closer); ok {
			_ = closer.Close()
		}
	})
}

func runSession(command *cobra.Command, args []string) error {
	modeText, _ := command.Flags().GetString("mode")
	mode := session.Mode(modeText)
	if !mode.Valid() {
		return fmt.Errorf("unknown interaction mode %q", mode)
	}
	timeout, _ := command.Flags().GetDuration("timeout")
	if timeout <= 0 || timeout > 30*time.Minute {
		return fmt.Errorf("session timeout must be positive and at most 30 minutes")
	}
	ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	state, err := open(command)
	if err != nil {
		return err
	}
	defer state.Close()
	installs, err := installsOf(command)
	if err != nil {
		return err
	}
	body := ""
	if len(args) > 1 {
		body = args[1]
	}
	m, body, err := sessionMappingOf(command, mode, body)
	if err != nil {
		return err
	}
	var piped *httpadapter.ClientSession
	var stream *session.Stream
	if m.http && !m.ws {
		pipes, err := dialConfig(command)
		if err != nil {
			return err
		}
		piped, err = httpadapter.OpenSession(ctx, pipes, body, func(ctx context.Context, request string) (*session.Stream, error) {
			return state.OpenSessionAddress(ctx, installs, args[0], request, mode)
		})
		if err != nil {
			return err
		}
		stream = piped.Stream
		defer func() { _ = piped.Close() }()
	} else {
		stream, err = state.OpenSessionAddress(ctx, installs, args[0], body, mode)
		if err != nil {
			return err
		}
	}
	defer func() { _ = stream.Close() }()
	return m.run(command, stream, piped, mode)
}

// sessionMapping is how arc session maps the session to the terminal:
// plain bytes, HTTP, WebSocket, or Exec with or without a terminal.
type sessionMapping struct {
	http, ws, exec, tty bool
}

// sessionMappingOf reads the flags of the mapping, checks them against the
// mode, and gives the first request of the mapping.
func sessionMappingOf(command *cobra.Command, mode session.Mode, body string) (sessionMapping, string, error) {
	var m sessionMapping
	m.http, _ = command.Flags().GetBool("http")
	m.ws, _ = command.Flags().GetBool("websocket")
	m.exec, _ = command.Flags().GetBool("exec")
	m.tty, _ = command.Flags().GetBool("tty")
	var err error
	if (m.exec || m.tty) && (m.http || m.ws) {
		return m, "", fmt.Errorf("choose one session output mapping")
	}
	if m.http || m.ws {
		if mode == session.RequestReply {
			return m, "", fmt.Errorf("HTTP streaming needs server_stream or duplex")
		}
		if m.ws {
			if mode != session.Duplex {
				return m, "", fmt.Errorf("WebSockets need duplex mode")
			}
			body, err = websocketRequest(body)
			if err != nil {
				return m, "", err
			}
		}
	}
	if m.exec || m.tty {
		if mode == session.RequestReply {
			return m, "", fmt.Errorf("exec I/O needs server_stream or duplex")
		}
		if m.tty {
			if mode != session.Duplex {
				return m, "", fmt.Errorf("a terminal needs duplex mode")
			}
			body, err = terminalRequest(command.InOrStdin(), body)
			if err != nil {
				return m, "", err
			}
		}
	}
	return m, body, nil
}

// run copies the session to and from the terminal, as the mapping says.
func (m sessionMapping) run(command *cobra.Command, stream *session.Stream, piped *httpadapter.ClientSession, mode session.Mode) error {
	if m.exec || m.tty {
		return execSession(command, stream, m.tty)
	}
	if m.ws {
		return websocketCLI(command, stream)
	}
	if mode == session.Duplex && m.http {
		// The HTTP session sends standard input as the request body, on
		// the pipe or in the session input.
		piped.Body = command.InOrStdin()
		defer closeOnEnd(stream, piped.Body)()
	} else if mode == session.Duplex {
		input := command.InOrStdin()
		defer closeOnEnd(stream, input)()
		go func() {
			_, err := io.Copy(stream, input)
			if err == nil {
				err = stream.CloseWrite()
			}
			if err != nil {
				_ = stream.Close()
			}
		}()
	}
	if m.http {
		return httpSessionCLI(command, piped)
	}
	_, err := io.Copy(command.OutOrStdout(), stream)
	if err == nil {
		err = stream.Wait()
	}
	return err
}
