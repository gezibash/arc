package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestBluetoothCLI keeps command parsing, runtime composition, Noise, ARC
// identity proofs, framing, stores, mail and provider dispatch real. Only the
// operating-system radio is replaced. This is not a hardware test.
func TestBluetoothCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test radio uses Unix sockets")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	rootPath, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	replacement := filepath.Join(rootPath, "cmd/arc/testdata/ble_radio.go.txt")
	platform := "direct_other.go"
	if runtime.GOOS == "linux" {
		platform = "direct_linux.go"
	}
	overlay, _ := json.Marshal(map[string]any{"Replace": map[string]string{filepath.Join(rootPath, "adapters/transport/ble", platform): replacement}})
	overlayPath := filepath.Join(temp, "overlay.json")
	if err = os.WriteFile(overlayPath, overlay, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(temp, "arc")
	build := exec.CommandContext(ctx, "go", "build", "-overlay", overlayPath, "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	provider := filepath.Join(temp, "echo")
	build = exec.CommandContext(ctx, "go", "build", "-o", provider, "../../adapters/provider/host/testdata/echo")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("provider: %v\n%s", err, out)
	}
	// Keep the socket path short enough for macOS sockaddr_un.
	socketDir, err := os.MkdirTemp("", "arc-ble-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	env := append(os.Environ(), "ARC_TEST_BLE_SOCKET="+filepath.Join(socketDir, "radio"))
	run := func(home string, flags []string, args ...string) string {
		t.Helper()
		argv := append([]string{"--home", home}, flags...)
		argv = append(argv, args...)
		cmd := exec.CommandContext(ctx, binary, argv...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	a, b := filepath.Join(temp, "alice"), filepath.Join(temp, "bob")
	run(a, nil, "keys", "gen")
	run(b, nil, "keys", "gen")
	alice, bob := strings.Split(run(a, nil, "whoami"), "\n")[1], strings.Split(run(b, nil, "whoami"), "\n")[1]
	// If any normal relay path is accidentally selected it cannot make this test pass.
	var network atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		network.Add(1)
		http.Error(w, "no relay", http.StatusServiceUnavailable)
	}))
	defer trap.Close()
	for _, home := range []string{a, b} {
		run(home, nil, "relay", "add", "ws"+strings.TrimPrefix(trap.URL, "http"))
	}
	network.Store(0) // relay add publishes lists before Bluetooth is selected.
	defer func() {
		if n := network.Load(); n != 0 {
			t.Errorf("Bluetooth attempted internet relay access %d times", n)
		}
	}()
	serverFlags := []string{"--bluetooth-peer", alice, "--bluetooth-listen"}
	clientFlags := []string{"--bluetooth-peer", bob, "--bluetooth-address", "00:11:22:33:44:55"}
	manifest := filepath.Join(rootPath, "cmd/arc/testdata/ble-echo.json")
	argv := append([]string{"--home", b}, serverFlags...)
	argv = append(argv, "serve", "exec://"+provider+"?manifest="+manifest)
	server := exec.CommandContext(ctx, binary, argv...)
	server.Env = env
	output := &output{}
	server.Stdout = output
	server.Stderr = output
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Process.Kill(); _ = server.Wait(); t.Log(output.String()) }()
	discovered := run(a, clientFlags, "discover")
	if !strings.Contains(discovered, "echo") {
		t.Fatalf("no announcement: %s", discovered)
	}
	run(a, clientFlags, "install", bob, "echo", "--yes")
	// The installed command goes through dispatch, which reads the Bluetooth flags itself.
	if result := run(a, clientFlags, "echo", "say", "installed"); !strings.Contains(result, "installed") {
		t.Fatalf("wrong reply from the installed command: %s", result)
	}
	for i := range 3 {
		body := fmt.Sprintf("hello-%d", i)
		result := run(a, clientFlags, "call", "--raw", "echo+arc://"+bob+"/", body)
		if !strings.Contains(result, body) {
			t.Fatalf("wrong reply: %s", result)
		}
	}
	run(a, clientFlags, "message", "send", bob, "private hello")
	run(a, clientFlags, "sync")
	run(a, clientFlags, "sync")
	if outbox := run(a, nil, "message", "outbox"); !strings.Contains(outbox, "delivered") {
		t.Fatalf("missing recipient receipt: %s", outbox)
	}
	// Stop the provider to inspect the recipient's persisted inbox using its normal command.
	_ = server.Process.Signal(os.Interrupt)
	if err = server.Wait(); err != nil {
		t.Fatalf("serve: %v\n%s", err, output.String())
	}
	inbox := run(b, nil, "message", "inbox")
	// Start the same provider again and make an explicit new call.
	server = exec.CommandContext(ctx, binary, argv...)
	server.Env = env
	server.Stdout = output
	server.Stderr = output
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	if result := run(a, clientFlags, "call", "echo+arc://"+bob+"/", "after restart"); !strings.Contains(result, "after restart") {
		t.Fatal(result)
	}
	if strings.Count(inbox, "private hello") != 1 {
		t.Fatalf("mail was missing or duplicated: %s", inbox)
	}
}

func TestBluetoothOptionsFailBeforeOpeningRadio(t *testing.T) {
	home := t.TempDir()
	ok(t, home, "", "keys", "gen")
	for _, args := range [][]string{
		{"--bluetooth-address", "00:11:22:33:44:55", "discover"},
		{"--bluetooth-peer", "bad", "discover"},
		{"--bluetooth-peer", strings.Repeat("1", 64), "discover"},
		{"--bluetooth-peer", strings.Repeat("1", 64), "--bluetooth-listen", "sync"},
		{"--bluetooth-peer", strings.Repeat("1", 64), "--bluetooth-address", "00:11:22:33:44:55", "sync", "--dir", t.TempDir()},
	} {
		if _, err := run(t, home, "", args...); err == nil {
			t.Fatalf("accepted unsupported configuration: %v", args)
		}
	}
}
