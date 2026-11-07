package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gezibash/arc/sdk/terminal"
)

// serveWait is how long arc serve waits for another serve of the same
// capability in the same home to stop, as during a restart.
var serveWait = 10 * time.Second

// holdServe takes the lock of one capability in the identity folder dir,
// so that two arc serve of one home never both run a request (#447). The
// system frees it when the process ends, also on SIGKILL. While another
// serve holds it, holdServe says so once on errs, and waits serveWait.
func holdServe(errs io.Writer, dir, id string) (release func(), err error) {
	sum := sha256.Sum256([]byte(id))
	path := filepath.Join(dir, "serve-"+hex.EncodeToString(sum[:8])+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("arc cannot open the lock of the serve of %s, %s: %w", terminal.EscapeLine(id), terminal.EscapeLine(path), err)
	}
	deadline := time.Now().Add(serveWait)
	for told := false; ; {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return nil, fmt.Errorf("arc cannot take the lock of the serve of %s: %w", terminal.EscapeLine(id), err)
		}
		holder := servedBy(file)
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("another arc serve of %s in this home runs%s, and did not stop within %s: stop it first, so that no request runs twice", terminal.EscapeLine(id), holder, serveWait)
		}
		if !told {
			fmt.Fprintln(errs, "arc: info: waiting for another arc serve of "+terminal.EscapeLine(id)+" in this home"+holder+" to stop")
			told = true
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The process ID tells the next serve which one holds the lock.
	if err := file.Truncate(0); err == nil {
		_, _ = file.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	}
	return func() { _ = file.Close() }, nil
}

// servedBy names the process that holds the lock, as " (pid N)", or "".
func servedBy(file *os.File) string {
	body := make([]byte, 16)
	n, _ := file.ReadAt(body, 0)
	if pid, err := strconv.Atoi(strings.TrimSpace(string(body[:n]))); err == nil && pid > 0 {
		return fmt.Sprintf(" (pid %d)", pid)
	}
	return ""
}
