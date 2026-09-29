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
	"github.com/spf13/cobra"
)

func sessionCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "session <address> [initial request]",
		Short: "Open an installed capability's live interaction",
		Long: "Open a core ARC session. Duplex sessions read ongoing input from stdin\n" +
			"and write provider output as it arrives. Input EOF allows final output.\n" +
			"The provider must declare the selected mode; a disconnect ends the session.",
		Args: cobra.RangeArgs(1, 2),
		RunE: runSession,
	}
	command.Flags().Bool("http", false, "decode streaming HTTP response bodies")
	command.Flags().Bool("websocket", false, "send input lines as WebSocket text messages")
	command.Flags().Bool("exec", false, "decode Exec output records and encode stdin")
	command.Flags().Bool("tty", false, "use an Exec PTY and a raw local terminal")
	command.Flags().String("mode", string(session.Duplex), "request_reply, server_stream, or duplex")
	command.Flags().Duration("timeout", 2*time.Minute, "session lifetime, at most 30 minutes")
	return command
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
	httpMode, _ := command.Flags().GetBool("http")
	wsMode, _ := command.Flags().GetBool("websocket")
	execMode, _ := command.Flags().GetBool("exec")
	tty, _ := command.Flags().GetBool("tty")
	if (execMode || tty) && (httpMode || wsMode) {
		return fmt.Errorf("choose one session output mapping")
	}
	if httpMode || wsMode {
		if mode == session.RequestReply {
			return fmt.Errorf("HTTP streaming needs server_stream or duplex")
		}
		if wsMode {
			if mode != session.Duplex {
				return fmt.Errorf("WebSockets need duplex mode")
			}
			body, err = websocketRequest(body)
			if err != nil {
				return err
			}
		}
	}
	if execMode || tty {
		if mode == session.RequestReply {
			return fmt.Errorf("Exec I/O needs server_stream or duplex")
		}
		if tty {
			if mode != session.Duplex {
				return fmt.Errorf("a terminal needs duplex mode")
			}
			body, err = terminalRequest(command.InOrStdin(), body)
			if err != nil {
				return err
			}
		}
	}
	stream, err := state.OpenSessionAddress(ctx, installs, args[0], body, mode)
	if err != nil {
		return err
	}
	defer stream.Close()
	if execMode || tty {
		return execSession(command, stream, tty)
	}
	if wsMode {
		return websocketCLI(command, stream)
	}
	if mode == session.Duplex {
		input := command.InOrStdin()
		// Owned pipe input can be interrupted when the session closes.
		interrupt := context.AfterFunc(stream.Context(), func() {
			if closer, ok := input.(io.Closer); ok {
				_ = closer.Close()
			}
		})
		defer interrupt()
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
	if httpMode {
		return httpSessionCLI(command, stream)
	}
	_, err = io.Copy(command.OutOrStdout(), stream)
	if err == nil {
		err = stream.Wait()
	}
	return err
}
