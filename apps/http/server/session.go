package server

import (
	"encoding/json"
	"github.com/gezibash/arc/sdk/provider"
	"io"
	"net/http"
)

type sessionHTTPKey struct{}

// POST /session opens a nested capability session. Query parameters contain
// address, mode and initial body. The HTTP body is the duplex input stream.
// An error trailer carries the final outcome after streaming headers are sent.
func (a *adapter) sessionCall(w http.ResponseWriter, r *http.Request) {
	mode := provider.Mode(r.URL.Query().Get("mode"))
	if mode == "" {
		mode = provider.ServerStream
	}
	address, body := r.URL.Query().Get("address"), r.URL.Query().Get("body")
	if address == "" || !mode.Valid() || len(body) > provider.MaxChunk {
		answer(w, 400, "error", "invalid session request")
		return
	}
	a.mu.Lock()
	caller := a.sessionCaller
	a.mu.Unlock()
	if caller == nil {
		answer(w, 503, "error", "the provider does not serve yet")
		return
	}
	stream, err := caller.OpenSession(r.Context(), address, body, mode)
	if err != nil {
		answer(w, 502, "error", err.Error())
		return
	}
	defer stream.Close()
	_ = http.NewResponseController(w).EnableFullDuplex()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Trailer", "Arc-Session-Error")
	w.WriteHeader(200)
	_ = http.NewResponseController(w).Flush()
	if mode == provider.Duplex {
		go func() {
			_, err := io.Copy(stream, r.Body)
			if err == nil {
				err = stream.CloseWrite()
			}
			if err != nil {
				stream.Close()
			}
		}()
	}
	_, err = io.Copy(flushWriter{w}, stream)
	if err == nil {
		err = stream.Wait()
	}
	if err != nil {
		data, _ := json.Marshal(err.Error())
		w.Header().Set("Arc-Session-Error", string(data))
	}
}

type flushWriter struct{ http.ResponseWriter }

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err == nil {
		err = http.NewResponseController(w.ResponseWriter).Flush()
	}
	return n, err
}
