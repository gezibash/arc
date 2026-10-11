package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/runtime/iface"
	"github.com/gezibash/arc/sdk/direct"
	httpadapter "github.com/gezibash/arc/sdk/httpadapter"
	"github.com/spf13/cobra"
)

func websocketRequest(body string) (string, error) {
	request := map[string]any{}
	if body != "" {
		if err := json.Unmarshal([]byte(body), &request); err != nil || request == nil {
			return "", fmt.Errorf("WebSocket request must be a JSON object")
		}
	}
	request["websocket"] = true
	data, err := json.Marshal(request)
	return string(data), err
}

// httpSessionCLI writes the response body to standard output, and the path
// of the body on standard error.
func httpSessionCLI(cmd *cobra.Command, piped *httpadapter.ClientSession) error {
	response, err := piped.Copy(cmd.OutOrStdout())
	if err != nil {
		return err
	}
	if failure := response.Trailers.Get("Arc-Session-Error"); failure != "" {
		var message string
		if json.Unmarshal([]byte(failure), &message) != nil {
			message = failure
		}
		return fmt.Errorf("nested session failed: %s", message)
	}
	fmt.Fprintln(cmd.ErrOrStderr(), direct.Report(piped.Path, response.Bytes, response.Elapsed))
	if response.Status >= 400 {
		return iface.ExitError{Code: 22}
	}
	return nil
}
func websocketCLI(cmd *cobra.Command, stream *session.Stream) error {
	input := cmd.InOrStdin()
	defer closeOnEnd(stream, input)()
	go func() {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), httpadapter.MaxHTTPBody)
		encoder := json.NewEncoder(stream)
		for scanner.Scan() {
			if err := encoder.Encode(httpadapter.SessionRecord{Type: "text", Text: scanner.Text()}); err != nil {
				return
			}
		}
		if scanner.Err() != nil {
			_ = stream.Close()
			return
		}
		_ = stream.CloseWrite()
	}()
	reader := httpadapter.NewSessionReader(stream)
	opened, closed := false, false
	for {
		record, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch record.Type {
		case "response":
			if record.Status != 101 {
				return fmt.Errorf("WebSocket handshake returned HTTP %d", record.Status)
			}
			opened = true
		case "text":
			if !opened || closed {
				return fmt.Errorf("WebSocket data outside open connection")
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), record.Text)
		case "binary":
			if !opened || closed {
				return fmt.Errorf("WebSocket data outside open connection")
			}
			_, err = cmd.OutOrStdout().Write(record.Data)
		case "pong":
		case "ws_close":
			closed = true
		default:
			return fmt.Errorf("unknown WebSocket record %q", record.Type)
		}
		if err != nil {
			return err
		}
	}
	if err := stream.Wait(); err != nil {
		return err
	}
	if !opened || !closed {
		return fmt.Errorf("incomplete WebSocket session")
	}
	return nil
}
