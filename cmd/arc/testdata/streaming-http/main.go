// Command streaming-http demonstrates an ordinary HTTP app hosted over ARC.
package main

import (
	"fmt"
	"github.com/coder/websocket"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "data: second\n\n")
	})
	mux.HandleFunc("/socket", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if err = conn.Write(r.Context(), websocket.MessageText, []byte("ready")); err != nil {
			return
		}
		for {
			kind, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if err = conn.Write(r.Context(), kind, data); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).EnableFullDuplex()
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				w.(http.Flusher).Flush()
			}
			if err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/nested", func(w http.ResponseWriter, r *http.Request) {
		endpoint := os.Getenv("ARC_CALL_URL")
		endpoint = endpoint[:len(endpoint)-len("/call")] + "/session?" + r.URL.RawQuery
		request, err := http.NewRequestWithContext(r.Context(), "POST", endpoint, nil)
		if err != nil {
			http.Error(w, "invalid nested request", 400)
			return
		}
		request.Header.Set("Authorization", "Bearer "+os.Getenv("ARC_CALL_TOKEN"))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			http.Error(w, "nested call failed", 502)
			return
		}
		defer response.Body.Close()
		w.Header().Set("Trailer", "Arc-Session-Error")
		w.WriteHeader(response.StatusCode)
		buf := make([]byte, 4096)
		for {
			n, err := response.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				w.(http.Flusher).Flush()
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				w.Header().Set("Arc-Session-Error", "read failed")
				return
			}
		}
		w.Header().Set("Arc-Session-Error", response.Trailer.Get("Arc-Session-Error"))
	})
	// /big?bytes=N sends N bytes: byte i is i mod 251.
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
		if err != nil || n < 0 {
			http.Error(w, "bytes=N", 400)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		block := make([]byte, 251*64)
		for i := range block {
			block[i] = byte(i % 251)
		}
		for n > 0 {
			m := min(n, int64(len(block)))
			if _, err := w.Write(block[:m]); err != nil {
				return
			}
			n -= m
		}
	})
	if err := http.ListenAndServe("127.0.0.1:"+os.Getenv("PORT"), mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
