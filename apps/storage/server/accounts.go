package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/gezibash/arc/sdk/provider"
	wire "github.com/gezibash/arc/sdk/storage"
	"github.com/gezibash/arc/sdk/terminal"
)

// accountOf finds the account of the author of a call. An admitted
// account key uses its own account. A device key uses the admitted account
// whose newest grant set holds it. A closed account takes no session. The
// app reads the records from the disk on each call, so a change of the
// operator counts at once.
func (s *server) accountOf(author string) (string, error) {
	active, err := s.store.active(author)
	if err != nil || active {
		return author, err
	}
	names, err := s.store.names()
	if err != nil {
		return "", err
	}
	for _, name := range names {
		granted, err := s.store.granted(name, author)
		if err != nil {
			return "", err
		}
		if !granted {
			continue
		}
		if active, err := s.store.active(name); err != nil || active {
			return name, err
		}
	}
	return "", errNoGrant
}

// join answers a request for an account. The author is the account key.
// The reply is an account, admitted or pending, or the error refused, in
// the body of the reply.
func (s *server) join(request provider.Request) (string, error) {
	var req wire.JoinCall
	j := &req.Join
	if json.Unmarshal([]byte(request.Message), &req) != nil || req.Version != 1 || j.Want < 0 || len(j.Note) > wire.MaxNote ||
		(j.Invite != "" && !wire.IsToken(j.Invite)) || (j.Citizen == "") != (j.Sig == "") {
		return "", errInvalidRequest
	}
	if j.Citizen != "" {
		if err := wire.VerifyJoin(j.Citizen, j.Sig, request.From, s.self); err != nil {
			fmt.Fprintf(s.log, "join of %.8s: %s\n", request.From, terminal.EscapeLine(err.Error()))
			return "", errInvalidRequest
		}
	}
	reply, err := s.store.join(request.From, req.Join, s.rules)
	if err != nil {
		fmt.Fprintf(s.log, "join of %.8s: %s\n", request.From, terminal.EscapeLine(err.Error()))
		return "", errFailed
	}
	status := reply.Error
	if reply.Account != nil {
		status = reply.Account.Status
	}
	fmt.Fprintf(s.log, "join of %.8s: %s\n", request.From, status) //arc:raw-ok an error code or an account state of this app
	raw, err := json.Marshal(reply)
	if err != nil {
		return "", errFailed
	}
	return string(raw), nil
}

// close ends the account of the author: the citizen leaves. Only an
// admitted account key closes its account, and only an account that holds
// no blob. The app ends each pipe of the account, and removes it at once.
func (s *server) close(request provider.Request) (string, error) {
	var req wire.CloseCall
	if json.Unmarshal([]byte(request.Message), &req) != nil || req.Version != 1 {
		return "", errInvalidRequest
	}
	reply := wire.CloseReply{Version: 1, Closed: true}
	// A refused close leaves the pipes of the account open. The removal
	// checks again, because a session can put until its pipe ends.
	_, err := s.store.leaving(request.From)
	if err == nil {
		err = s.removing(request.From, func() error { return s.store.leave(request.From) })
	}
	if r, ok := errors.AsType[refusal](err); ok {
		reply = wire.CloseReply{Version: 1, Error: r.code, Message: r.message}
		err = nil
	}
	if errors.Is(err, errNoGrant) {
		return "", errNotOwner
	}
	if err != nil {
		fmt.Fprintf(s.log, "close of %.8s: %s\n", request.From, terminal.EscapeLine(err.Error()))
		return "", errFailed
	}
	if reply.Closed {
		fmt.Fprintf(s.log, "close of %.8s: the account is removed\n", request.From)
	} else {
		fmt.Fprintf(s.log, "close of %.8s: %s\n", request.From, terminal.EscapeLine(reply.Message))
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		return "", errFailed
	}
	return string(raw), nil
}

// sweep removes the bytes of each closed account. It runs at the start of
// the service and at each call, so a closed account loses its bytes at the
// first call after its reads end.
func (s *server) sweep() {
	names, err := s.store.closed()
	if err != nil {
		fmt.Fprintf(s.log, "the sweep of closed accounts: %s\n", terminal.EscapeLine(err.Error()))
		return
	}
	for _, name := range names {
		var removed bool
		err := s.removing(name, func() (err error) {
			removed, err = s.store.removeClosed(name)
			return err
		})
		switch {
		case err != nil:
			fmt.Fprintf(s.log, "account %.8s is closed, and its removal failed: %s\n", name, terminal.EscapeLine(err.Error()))
		case removed:
			fmt.Fprintf(s.log, "account %.8s is closed: its bytes are removed\n", name)
		}
	}
}

// hold returns the lock of an account.
func (s *server) hold(account string) *sync.RWMutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holds == nil {
		s.holds = map[string]*sync.RWMutex{}
	}
	if s.holds[account] == nil {
		s.holds[account] = &sync.RWMutex{}
	}
	return s.holds[account]
}

// removing ends each pipe of an account, waits until no session holds
// the account, and then runs remove.
func (s *server) removing(account string, remove func() error) error {
	s.mu.Lock()
	var ends []func()
	for end := range s.pipes[account] {
		ends = append(ends, *end)
	}
	s.mu.Unlock()
	for _, end := range ends {
		end()
	}
	hold := s.hold(account)
	hold.Lock()
	defer hold.Unlock()
	return remove()
}

// track keeps the end of a pipe of an account until the pipe ends.
func (s *server) track(account string, end *func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pipes == nil {
		s.pipes = map[string]map[*func()]struct{}{}
	}
	if s.pipes[account] == nil {
		s.pipes[account] = map[*func()]struct{}{}
	}
	s.pipes[account][end] = struct{}{}
}

func (s *server) untrack(account string, end *func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pipes[account], end)
	if len(s.pipes[account]) == 0 {
		delete(s.pipes, account)
	}
}

// grant sets the grant set of the account of the author. Only an admitted
// account key sets grants.
func (s *server) grant(request provider.Request) (string, error) {
	var req wire.Grants
	if json.Unmarshal([]byte(request.Message), &req) != nil || req.Version != 1 || req.At <= 0 || len(req.Devices) > wire.MaxGrants {
		return "", errInvalidRequest
	}
	for _, device := range req.Devices {
		if !wire.IsID(device) {
			return "", errInvalidRequest
		}
	}
	if active, err := s.store.active(request.From); err != nil || !active {
		if err != nil {
			fmt.Fprintf(s.log, "account %.8s: %s\n", request.From, terminal.EscapeLine(err.Error()))
			return "", errFailed
		}
		return "", errNotOwner
	}
	if err := s.store.grant(request.From, req.Devices, req.At); err != nil {
		if r, ok := errors.AsType[refusal](err); ok {
			return "", provider.Error(r.code)
		}
		fmt.Fprintf(s.log, "account %.8s: %s\n", request.From, terminal.EscapeLine(err.Error()))
		return "", errFailed
	}
	fmt.Fprintf(s.log, "account %.8s: %d device keys have a grant\n", request.From, len(req.Devices))
	raw, err := json.Marshal(wire.Granted{Version: 1, Devices: len(req.Devices)})
	if err != nil {
		return "", errFailed
	}
	return string(raw), nil
}
