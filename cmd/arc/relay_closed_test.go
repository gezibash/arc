package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// closingRelay takes each WebSocket upgrade, answers it, and then ends the
// connection as how says: "fin" closes it, "rst" resets it, and "read"
// reads one byte and then resets it. It counts the upgrades.
func closingRelay(t *testing.T, how string) (string, *atomic.Int64) {
	t.Helper()
	var upgrades atomic.Int64
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			http.NotFound(w, r)
			return
		}
		upgrades.Add(1)
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) //nolint:gosec // RFC 6455 makes the accept key with SHA-1
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
		_ = buf.Flush()
		if how == "read" {
			_, _ = buf.ReadByte()
		}
		if how != "fin" {
			_ = conn.(*net.TCPConn).SetLinger(0)
		}
		_ = conn.Close()
	})}
	go func() { _ = server.Serve(l) }()
	t.Cleanup(func() { _ = server.Close() })
	return "ws://" + l.Addr().String(), &upgrades
}

// A relay that takes the WebSocket upgrade and then closes or resets the
// connection gives one line, as a host that closes it before the upgrade
// does, and one sync dials it once (#468). The test failed on main before
// the fix: four raw lines, and six upgrades.
func TestARelayThatClosesAfterTheUpgradeGivesOneLine(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"fin", "rst", "read"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			url, upgrades := closingRelay(t, how)
			home := t.TempDir()
			ok(t, home, "", "keys", "gen")
			ok(t, home, "", "relay", "add", url)
			before := upgrades.Load()
			var out, errs bytes.Buffer
			command := root()
			command.SetOut(&out)
			command.SetErr(&errs)
			command.SetArgs([]string{"--home", home, "sync"})
			err := command.Execute()
			want := url + ": the connection was closed; arc syncs with it again at the next sync"
			if err == nil || errorLine(err) != "arc: "+want || errs.Len() != 0 || strings.Contains(out.String(), url) {
				t.Errorf("arc sync printed %q, %q and %v, want only %q", out.String(), errs.String(), err, want)
			}
			if n := upgrades.Load() - before; n != 1 {
				t.Errorf("arc sync made %d upgrades, want 1", n)
			}
			// A close at once saves no wait, so the other processes of the
			// home never skip the relay for it (#444).
			_ = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
				if err == nil && d.Name() == "relays-down.json" {
					if data, _ := os.ReadFile(path); strings.Contains(string(data), url) {
						t.Errorf("arc sync marked the relay down for the home: %s", data)
					}
				}
				return nil
			})
		})
	}
}
