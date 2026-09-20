package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gezibash/arc/go/provider"
)

// lostGrace is how long a job may hold no result after its process ended. The
// provider writes the result just after the process ends, so a job is lost
// only when nothing arrives inside this window.
const lostGrace = 5 * time.Second

// jobStatus is the file that a job keeps on disk.
type jobStatus struct {
	Owner     string `json:"owner"`
	State     string `json:"state"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
	Exit      *int   `json:"exit,omitempty"`
	TimedOut  *bool  `json:"timed_out,omitempty"`
}

// startJob runs the command in the background. The job holds the lease until
// it ends, so the machine stays awake while it runs.
func (s *server) startJob(caller string, cmd *command) (map[string]any, error) {
	id, err := newJobID()
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(s.config.JobsDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	stdout, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		return nil, err
	}
	stderr, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		stdout.Close()
		return nil, err
	}

	process := start(cmd)
	process.Stdin = readerOf(cmd.Stdin)
	process.Stdout = stdout
	process.Stderr = stderr

	s.lease.hold()
	if err := process.Start(); err != nil {
		s.lease.release()
		stdout.Close()
		stderr.Close()
		return nil, err
	}

	status := jobStatus{
		Owner:     caller,
		State:     "running",
		PID:       process.Process.Pid,
		StartedAt: now(),
	}
	if err := writeStatus(dir, status); err != nil {
		return nil, err
	}

	go func() {
		defer s.lease.release()
		defer stdout.Close()
		defer stderr.Close()

		timedOut := wait(process, time.Duration(cmd.TimeoutMS)*time.Millisecond)
		exit := process.ProcessState.ExitCode()

		status.State = "done"
		status.EndedAt = now()
		status.Exit = &exit
		status.TimedOut = &timedOut
		if err := writeStatus(dir, status); err != nil {
			fmt.Fprintf(s.log, "the status of job %s did not save: %v\n", id, err)
		}

		// The notify command runs before the lease ends, so the machine stays
		// awake until the caller holds the result.
		if s.config.Notify != nil {
			reply, err := s.jobStatus(caller, id)
			if err != nil {
				fmt.Fprintf(s.log, "the result of job %s is not readable: %v\n", id, err)
				return
			}
			s.notify(caller, reply)
		}
	}()

	return map[string]any{"job": id, "state": "running"}, nil
}

// jobStatus reads the state and the output of one job. A job of another
// caller looks the same as a job that is not there.
func (s *server) jobStatus(caller, id string) (map[string]any, error) {
	dir := filepath.Join(s.config.JobsDir, id)
	path := filepath.Join(dir, "status.json")

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, provider.Error("not_found")
	}

	var status jobStatus
	if err := json.Unmarshal(data, &status); err != nil || status.Owner != caller {
		return nil, provider.Error("not_found")
	}

	if status.State == "running" && !alive(status.PID) {
		if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) > lostGrace {
			status.State = "lost"
		}
	}

	// The end of the output matters most for a long job, so the reply carries
	// the tail of each stream.
	limit := s.config.Limits.OutputBytes
	stdout, outClipped := tail(filepath.Join(dir, "stdout"), limit)
	stderr, errClipped := tail(filepath.Join(dir, "stderr"), max(0, limit-len(stdout)))

	reply := map[string]any{
		"job":       id,
		"state":     status.State,
		"stdout":    safeText(stdout),
		"stderr":    safeText(stderr),
		"truncated": outClipped || errClipped,
	}
	if status.StartedAt != "" {
		reply["started_at"] = status.StartedAt
	}
	if status.EndedAt != "" {
		reply["ended_at"] = status.EndedAt
	}
	if status.Exit != nil {
		reply["exit"] = *status.Exit
	}
	if status.TimedOut != nil {
		reply["timed_out"] = *status.TimedOut
	}
	return reply, nil
}

// writeStatus writes the file under another name and renames it, so a reader
// never sees half a file.
func writeStatus(dir string, status jobStatus) error {
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}

	temporary := filepath.Join(dir, "status.json.tmp")
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(dir, "status.json"))
}

// tail reads the last bytes of a file.
func tail(path string, limit int) ([]byte, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()

	size, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, false
	}

	at := max(0, size-int64(limit))
	if _, err := file.Seek(at, io.SeekStart); err != nil {
		return nil, false
	}

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, false
	}
	return data, size > int64(limit)
}

// alive says whether the process is still there.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func newJobID() (string, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	return fmt.Sprintf("%012x-%s", time.Now().UnixMilli(), hex.EncodeToString(suffix)), nil
}

func now() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
