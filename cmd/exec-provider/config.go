// Command exec-provider runs one command for each ARC request.
//
// The ARC runtime sends one JSON event for each line. Only the public keys in
// the configuration may run a command. Requests run at one time, so one slow
// command never holds up another caller.
//
// The field "action" of the body picks the operation:
//
//	run     run the command, and reply with the result. This is the default.
//	start   start the command as a job, and reply with its id at once.
//	status  reply with the state and the output of one job.
//
// When a job ends, the provider runs the notify command of the operator, if
// the configuration holds one. The result of the job goes to its standard
// input. This sends the result to the caller, for example as a direct message.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/gezibash/arc/provider"
)

// The limits of a request, and the ceiling of each one.
//
// An ARC reply waits at most 120 seconds. The command therefore stops before
// that, so the caller gets the output and not a transport timeout.
var (
	defaultLimits = limits{
		BodyBytes:    256 * 1024,
		OutputBytes:  1024 * 1024,
		TimeoutMS:    60_000,
		JobTimeoutMS: 3_600_000,
	}
	maxLimits = limits{
		BodyBytes:    1024 * 1024,
		OutputBytes:  4 * 1024 * 1024,
		TimeoutMS:    115_000,
		JobTimeoutMS: 86_400_000,
	}
)

var jobIDPattern = regexp.MustCompile(`^[0-9a-f]{12}-[0-9a-f]{8}$`)

type limits struct {
	BodyBytes    int `json:"body_bytes"`
	OutputBytes  int `json:"output_bytes"`
	TimeoutMS    int `json:"timeout_ms"`
	JobTimeoutMS int `json:"job_timeout_ms"`
}

type leaseConfig struct {
	Hold       []string `json:"hold"`
	Release    []string `json:"release"`
	IntervalMS int      `json:"interval_ms"`
}

type notifyConfig struct {
	Argv      []string `json:"argv"`
	TimeoutMS int      `json:"timeout_ms"`
}

type configFile struct {
	Grants  []string      `json:"grants"`
	CWD     string        `json:"cwd"`
	Limits  *limits       `json:"limits"`
	Lease   *leaseConfig  `json:"lease"`
	Notify  *notifyConfig `json:"notify"`
	JobsDir string        `json:"jobs_dir"`
}

// config holds the checked configuration of the provider.
type config struct {
	Grants  map[string]bool
	CWD     string
	Limits  limits
	Lease   *leaseConfig
	Notify  *notifyConfig
	JobsDir string
}

// loadConfig reads EXEC_CONFIG and checks every field. The provider fails
// closed: a configuration that is not complete stops the program.
func loadConfig() (*config, error) {
	path, err := provider.ConfigPath("EXEC_CONFIG")
	if err != nil {
		return nil, err
	}

	var file configFile
	if err := provider.ReadConfig(path, &file); err != nil {
		return nil, err
	}

	grants, err := provider.Grants(file.Grants)
	if err != nil {
		return nil, err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("the home directory is unknown: %v", err)
	}

	workingDir := file.CWD
	if workingDir == "" {
		workingDir = home
	}
	if err := provider.Directory("cwd", workingDir); err != nil {
		return nil, err
	}

	checked := defaultLimits
	if file.Limits != nil {
		checked = merge(*file.Limits, defaultLimits)
		if err := checkLimits(checked); err != nil {
			return nil, err
		}
	}

	lease, err := checkLease(file.Lease)
	if err != nil {
		return nil, err
	}

	notify, err := checkNotify(file.Notify)
	if err != nil {
		return nil, err
	}

	jobsDir := file.JobsDir
	if jobsDir == "" {
		jobsDir = filepath.Join(home, ".arc", "exec", "jobs")
	}
	if !filepath.IsAbs(jobsDir) {
		return nil, fmt.Errorf("jobs_dir must be an absolute path")
	}

	return &config{
		Grants:  grants,
		CWD:     workingDir,
		Limits:  checked,
		Lease:   lease,
		Notify:  notify,
		JobsDir: jobsDir,
	}, nil
}

// merge keeps the default of a limit that the file leaves out.
func merge(given, fallback limits) limits {
	if given.BodyBytes == 0 {
		given.BodyBytes = fallback.BodyBytes
	}
	if given.OutputBytes == 0 {
		given.OutputBytes = fallback.OutputBytes
	}
	if given.TimeoutMS == 0 {
		given.TimeoutMS = fallback.TimeoutMS
	}
	if given.JobTimeoutMS == 0 {
		given.JobTimeoutMS = fallback.JobTimeoutMS
	}
	return given
}

func checkLimits(given limits) error {
	for _, limit := range []struct {
		name  string
		value int
		max   int
	}{
		{"body_bytes", given.BodyBytes, maxLimits.BodyBytes},
		{"output_bytes", given.OutputBytes, maxLimits.OutputBytes},
		{"timeout_ms", given.TimeoutMS, maxLimits.TimeoutMS},
		{"job_timeout_ms", given.JobTimeoutMS, maxLimits.JobTimeoutMS},
	} {
		if err := provider.Limit(limit.name, limit.value, limit.max); err != nil {
			return err
		}
	}
	return nil
}

func checkLease(given *leaseConfig) (*leaseConfig, error) {
	if given == nil {
		return nil, nil
	}
	if len(given.Hold) == 0 || len(given.Release) == 0 {
		return nil, fmt.Errorf("lease.hold and lease.release must each hold one command")
	}
	if given.IntervalMS < 1000 || given.IntervalMS > 3_600_000 {
		return nil, fmt.Errorf("lease.interval_ms must be a whole number from 1000 to 3600000")
	}
	return given, nil
}

func checkNotify(given *notifyConfig) (*notifyConfig, error) {
	if given == nil {
		return nil, nil
	}
	if len(given.Argv) == 0 {
		return nil, fmt.Errorf("notify.argv must hold one command")
	}
	if given.TimeoutMS == 0 {
		given.TimeoutMS = 30_000
	}
	if given.TimeoutMS < 1000 || given.TimeoutMS > 300_000 {
		return nil, fmt.Errorf("notify.timeout_ms must be a whole number from 1000 to 300000")
	}
	return given, nil
}
