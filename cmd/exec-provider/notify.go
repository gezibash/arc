package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// notify tells the caller that a job ended. The result goes to the standard
// input of the command. The text {owner} in the command becomes the public
// key of the caller.
func (s *server) notify(owner string, result map[string]any) {
	config := s.config.Notify
	if config == nil {
		return
	}

	argv := make([]string, len(config.Argv))
	for index, part := range config.Argv {
		argv[index] = strings.ReplaceAll(part, "{owner}", owner)
	}

	body, err := encodeJSON(result)
	if err != nil {
		fmt.Fprintf(s.log, "the job result does not encode: %v\n", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.TimeoutMS)*time.Millisecond)
	defer cancel()

	process := exec.CommandContext(ctx, argv[0], argv[1:]...)
	process.Stdin = bytes.NewReader(body)

	var stderr bytes.Buffer
	process.Stderr = &stderr

	job, _ := result["job"].(string)
	if err := process.Run(); err != nil {
		fmt.Fprintf(s.log, "notify failed for job %s: %v: %s\n", job, err, trimOutput(stderr.Bytes()))
	}
}
