// Command dm-provider serves sealed direct messages with a mailbox that
// lasts.
//
// A body is sealed to its reader before it leaves the sender, so the
// provider holds ciphertext only. The mailbox lives under DM_ROOT.
package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/gezibash/arc/provider"
)

// server answers the commands of every caller.
type server struct {
	config *config
	store  *store

	mu     sync.Mutex
	events provider.Events
}

func main() {
	held, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	handler := &server{config: held, store: &store{root: held.Root}}

	if err := provider.Run(context.Background(), handler, provider.Options{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// SetEvents takes the writer that sends an event to another citizen.
func (s *server) SetEvents(events provider.Events) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = events
}

// HandleRequest answers one command.
func (s *server) HandleRequest(_ context.Context, request provider.Request) (string, error) {
	answer, err := s.run(request.From, request.Message)
	if err != nil {
		return "", provider.Error(err.Error())
	}

	// The events go out before the reply, so a citizen that watches sees the
	// message as soon as the sender does.
	s.mu.Lock()
	events := s.events
	s.mu.Unlock()

	if events != nil {
		for _, one := range answer.events {
			if err := events.Emit(one.To, one.Topic, one.Body, one.Meta); err != nil {
				fmt.Fprintf(os.Stderr, "the event %s did not reach %s: %v\n", one.Topic, one.To, err)
			}
		}
	}
	return answer.reply, nil
}
