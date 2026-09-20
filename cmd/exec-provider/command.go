package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/gezibash/arc/provider"
)

// command is one command to run.
type command struct {
	Argv      []string
	Dir       string
	Stdin     []byte
	TimeoutMS int
}

// body is the request that a caller sends.
type body struct {
	Action    string   `json:"action"`
	Argv      []string `json:"argv"`
	Script    string   `json:"script"`
	CWD       string   `json:"cwd"`
	Stdin     string   `json:"stdin"`
	TimeoutMS *int     `json:"timeout_ms"`
	Job       string   `json:"job"`
}

// parseRequest reads the body of a request. It returns the action, and either
// a command or a job id.
func parseRequest(cfg *config, message string) (action string, cmd *command, job string, err error) {
	if len(message) > cfg.Limits.BodyBytes {
		return "", nil, "", provider.Error("request_too_large")
	}

	decoder := json.NewDecoder(strings.NewReader(message))
	decoder.DisallowUnknownFields()

	var request body
	if err := decoder.Decode(&request); err != nil {
		return "", nil, "", provider.Error("invalid_request")
	}
	if decoder.More() {
		return "", nil, "", provider.Error("invalid_request")
	}

	if request.Action == "" {
		request.Action = "run"
	}

	switch request.Action {
	case "status":
		if !jobIDPattern.MatchString(request.Job) || request.Argv != nil || request.Script != "" {
			return "", nil, "", provider.Error("status needs exactly one valid job")
		}
		return "status", nil, request.Job, nil

	case "run", "start":
		if request.Job != "" {
			return "", nil, "", provider.Error("invalid_request")
		}

		limit := cfg.Limits.TimeoutMS
		if request.Action == "start" {
			limit = cfg.Limits.JobTimeoutMS
		}

		cmd, err := parseCommand(cfg, request, limit)
		if err != nil {
			return "", nil, "", err
		}
		return request.Action, cmd, "", nil

	default:
		return "", nil, "", provider.Error("action must be run, start, or status")
	}
}

func parseCommand(cfg *config, request body, timeoutLimitMS int) (*command, error) {
	argv := request.Argv
	switch {
	case (argv == nil) == (request.Script == ""):
		return nil, provider.Error("give exactly one of argv or script")
	case argv != nil:
		if len(argv) == 0 {
			return nil, provider.Error("argv must be a list of strings, and must hold one part")
		}
	default:
		argv = []string{"bash", "-lc", request.Script}
	}

	dir := cfg.CWD
	if request.CWD != "" {
		dir = resolve(cfg.CWD, request.CWD)
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			return nil, provider.Error("cwd is not a directory")
		}
	}

	timeout := timeoutLimitMS
	if request.TimeoutMS != nil {
		if *request.TimeoutMS < 1 {
			return nil, provider.Error("timeout_ms must be a whole number above zero")
		}
		timeout = min(*request.TimeoutMS, timeoutLimitMS)
	}

	return &command{
		Argv:      argv,
		Dir:       dir,
		Stdin:     []byte(request.Stdin),
		TimeoutMS: timeout,
	}, nil
}

// resolve joins a path of the request to the directory of the provider. An
// absolute path of the request stands on its own, as it does in the shell.
func resolve(base, path string) string {
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}
