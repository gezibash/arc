package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	httpadapter "github.com/gezibash/arc/adapters/http"
	"github.com/gezibash/arc/application/iface"
	"github.com/gezibash/arc/core/session"
	"github.com/spf13/cobra"
	"io"
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
func httpSessionCLI(cmd *cobra.Command, stream *session.Stream) error {
	reader := httpadapter.NewSessionReader(stream)
	status := 0
	ended := false
	for {
		record, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if ended {
			return fmt.Errorf("HTTP data after end")
		}
		switch record.Type {
		case "response":
			if status != 0 || record.Status < 200 || record.Status > 599 {
				return fmt.Errorf("invalid HTTP response status")
			}
			status = record.Status
		case "body":
			if status == 0 {
				return fmt.Errorf("HTTP body before headers")
			}
			if _, err = cmd.OutOrStdout().Write(record.Data); err != nil {
				return err
			}
		case "trailers":
		case "end":
			ended = true
		default:
			return fmt.Errorf("unknown HTTP record %q", record.Type)
		}
	}
	if err := stream.Wait(); err != nil {
		return err
	}
	if status == 0 || !ended {
		return fmt.Errorf("incomplete HTTP response")
	}
	if status >= 400 {
		return iface.ExitError{Code: 22}
	}
	return nil
}
func websocketCLI(cmd *cobra.Command, stream *session.Stream) error {
	input := cmd.InOrStdin()
	stop := context.AfterFunc(stream.Context(), func() {
		if c, ok := input.(io.Closer); ok {
			c.Close()
		}
	})
	defer stop()
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
			stream.Close()
			return
		}
		_ = stream.CloseWrite()
	}()
	reader := httpadapter.NewSessionReader(stream)
	opened, closed := false, false
	for {
		record, err := reader.Next()
		if err == io.EOF {
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
