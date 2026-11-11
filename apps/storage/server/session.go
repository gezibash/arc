package server

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/gezibash/arc/sdk/direct"
	wire "github.com/gezibash/arc/sdk/storage"
	"github.com/pion/webrtc/v4"
)

// session serves the requests of one pipe. It reads the messages in
// order, and answers each request in order. A client with FeatureBatch
// keeps several requests in flight, and each reply carries the N of its
// request.
//
// A put gets the reply send, and waits in sends. The client sends the
// bytes of each put in the order of the send replies, so each binary run
// belongs to the oldest put in sends. When a run is complete, its file is
// written, and the put waits in group. The session writes the entries of
// the group in one transaction when no message waits, before a request
// that is not a put, or when the window is full. Only then does each put
// get the reply stored.
type session struct {
	store    *store
	account  string
	dc       *webrtc.DataChannel
	messages <-chan webrtc.DataChannelMessage
	done     <-chan struct{}

	sends []sending
	group []written
}

// sending is a put that got the reply send, and waits for its bytes.
type sending struct {
	n    uint64
	id   string
	size int64
	tag  []byte
	// file takes the bytes as they come; got counts them.
	file *blobFile
	got  int64
}

// errClosed says that the client closed the pipe.
var errClosed = errors.New("closed")

func (s *session) next() (webrtc.DataChannelMessage, error) {
	select {
	case m := <-s.messages:
		return m, nil
	case <-s.done:
		return webrtc.DataChannelMessage{}, errClosed
	case <-time.After(IdleWait):
		return webrtc.DataChannelMessage{}, errors.New("the client sent nothing for 30 s")
	}
}

func (s *session) serve() error {
	// A put whose bytes did not all come leaves no file.
	defer func() {
		for _, p := range s.sends {
			if p.file != nil {
				p.file.abort()
			}
		}
	}()
	for {
		if len(s.group) > 0 && len(s.messages) == 0 {
			if err := s.commit(); err != nil {
				return err
			}
		}
		m, err := s.next()
		if errors.Is(err, errClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		if !m.IsString {
			if err := s.bytes(m.Data); err != nil {
				return err
			}
			continue
		}
		var req wire.Request
		if json.Unmarshal(m.Data, &req) != nil {
			return errors.New("the client sent a message that is not a request")
		}
		if req.Op != wire.OpPut && len(s.group) > 0 {
			if err := s.commit(); err != nil {
				return err
			}
		}
		if err := s.handle(req); err != nil {
			return err
		}
	}
}

// handle answers one request. It returns an error only if the pipe cannot
// go on.
func (s *session) handle(req wire.Request) error {
	switch req.Op {
	case wire.OpHello:
		return s.reply(wire.Reply{N: req.N, Version: 2, Features: []string{wire.FeatureBatch, wire.FeaturePieces, wire.FeatureAudit, wire.FeatureDrain}, Window: wire.Window})
	case wire.OpDrain:
		r, err := s.store.drain(s.account, req.Off)
		if err != nil {
			return s.refuse(req.N, err)
		}
		r.N = req.N
		return s.reply(r)
	case wire.OpList:
		limit := req.Limit
		if limit <= 0 || limit > wire.MaxList {
			limit = wire.MaxList
		}
		entries, next, err := s.store.list(s.account, req.After, limit)
		if err != nil {
			return s.refuse(req.N, err)
		}
		return s.reply(wire.Reply{N: req.N, Entries: entries, Next: next})
	case wire.OpUsage:
		r, err := s.store.usage(s.account)
		if err != nil {
			return s.refuse(req.N, err)
		}
		r.N = req.N
		return s.reply(r)
	}
	ids, ok := idsOf(req)
	if !ok {
		return s.reply(wire.Reply{N: req.N, Error: wire.ErrInvalidRequest, Message: fmt.Sprintf("a request names one blob ID, or 1 to %d blob IDs; a blob ID is 64 lower-case hex digits", wire.MaxBatch)})
	}
	switch req.Op {
	case wire.OpPut:
		if req.IDs != nil {
			return s.reply(wire.Reply{N: req.N, Error: wire.ErrInvalidRequest, Message: "a put names one blob ID"})
		}
		return s.put(req)
	case wire.OpGet:
		return s.get(req, ids)
	case wire.OpAudit:
		if !wire.IsNonce(req.Nonce) {
			return s.reply(wire.Reply{N: req.N, Error: wire.ErrInvalidRequest, Message: fmt.Sprintf("an audit has a nonce of %d bytes in lower-case hex", wire.NonceSize)})
		}
		nonce, _ := hex.DecodeString(req.Nonce)
		items, err := s.store.audit(s.account, ids, nonce)
		if err != nil {
			return s.refuse(req.N, err)
		}
		if req.IDs != nil {
			return s.reply(wire.Reply{N: req.N, Items: items})
		}
		if items[0].Error != "" {
			return s.reply(wire.Reply{N: req.N, Error: items[0].Error, Message: "the account holds no blob " + req.ID})
		}
		return s.reply(wire.Reply{N: req.N, Size: items[0].Size, Proof: items[0].Proof})
	case wire.OpDelete:
		items, err := s.store.remove(s.account, ids)
		if err != nil {
			return s.refuse(req.N, err)
		}
		if req.IDs != nil {
			return s.reply(wire.Reply{N: req.N, Items: items})
		}
		return s.reply(wire.Reply{N: req.N, Status: items[0].Status, Seq: items[0].Seq})
	}
	return s.reply(wire.Reply{N: req.N, Error: wire.ErrInvalidRequest, Message: "unknown operation " + strconv.Quote(req.Op)})
}

// idsOf returns the blob IDs of a request: its ID, or its IDs.
func idsOf(req wire.Request) ([]string, bool) {
	if req.IDs == nil {
		return []string{req.ID}, wire.IsID(req.ID)
	}
	if req.ID != "" || len(req.IDs) == 0 || len(req.IDs) > wire.MaxBatch {
		return nil, false
	}
	for _, id := range req.IDs {
		if !wire.IsID(id) {
			return nil, false
		}
	}
	return req.IDs, true
}

// get answers a get. With IDs, the reply gives each size, and the bytes of
// each found blob follow, in order.
func (s *session) get(req wire.Request, ids []string) error {
	if req.IDs == nil {
		data, err := s.store.get(s.account, req.ID)
		if err != nil {
			return s.refuse(req.N, err)
		}
		if err := s.reply(wire.Reply{N: req.N, Size: int64(len(data))}); err != nil {
			return err
		}
		return direct.Send(s.dc, bytes.NewReader(data), int64(len(data)))
	}
	items, err := s.store.sizes(s.account, ids)
	if err != nil {
		return s.refuse(req.N, err)
	}
	if err := s.reply(wire.Reply{N: req.N, Items: items}); err != nil {
		return err
	}
	for _, item := range items {
		if item.Error != "" {
			continue
		}
		// The reply named the size, so a file that does not match it ends
		// the pipe.
		data, err := os.ReadFile(s.store.blobPath(s.account, item.ID))
		if err != nil {
			return err
		}
		if int64(len(data)) != item.Size {
			return fmt.Errorf("the file of %s has %d bytes, and its entry %d", item.ID, len(data), item.Size)
		}
		if err := direct.Send(s.dc, bytes.NewReader(data), item.Size); err != nil {
			return err
		}
	}
	return nil
}

// put admits a blob. If its bytes can go, the put waits in sends.
func (s *session) put(req wire.Request) error {
	if len(s.sends)+len(s.group) >= wire.Window {
		return fmt.Errorf("the client has more than %d puts in flight", wire.Window)
	}
	var reserved int64
	for _, p := range s.sends {
		if p.id == req.ID {
			return s.reply(wire.Reply{N: req.N, Status: wire.StatusHeld})
		}
		reserved += p.size
	}
	for _, p := range s.group {
		if p.id == req.ID {
			return s.reply(wire.Reply{N: req.N, Status: wire.StatusHeld})
		}
		reserved += p.size
	}
	var tag []byte
	if req.Tag != "" {
		var err error
		if tag, err = base64.StdEncoding.DecodeString(req.Tag); err != nil || len(tag) != wire.TagSize {
			return s.reply(wire.Reply{N: req.N, Error: wire.ErrInvalidRequest, Message: fmt.Sprintf("the tag of a piece is %d bytes in standard base64", wire.TagSize)})
		}
	}
	status, err := s.store.admit(s.account, req.ID, req.Size, reserved, tag != nil)
	if err != nil {
		return s.refuse(req.N, err)
	}
	if status == wire.StatusSend {
		s.sends = append(s.sends, sending{n: req.N, id: req.ID, size: req.Size, tag: tag})
	}
	return s.reply(wire.Reply{N: req.N, Status: status})
}

// bytes takes one binary message for the oldest put in sends. When the
// bytes of the put are complete, it writes its file.
func (s *session) bytes(data []byte) error {
	if len(s.sends) == 0 {
		return errors.New("the client sent bytes for no put")
	}
	p := &s.sends[0]
	if p.got+int64(len(data)) > p.size {
		return errors.New("the client sent other bytes than the size of the put")
	}
	if p.file == nil {
		f, err := s.store.createBlob(s.account, p.id)
		if err != nil {
			return err
		}
		p.file = f
	}
	if err := p.file.write(data); err != nil {
		return err
	}
	p.got += int64(len(data))
	if p.got < p.size {
		return nil
	}
	head := s.sends[0]
	s.sends = s.sends[1:]
	if err := head.file.finish(); err != nil {
		return s.refuse(head.n, err)
	}
	s.group = append(s.group, written{n: head.n, id: head.id, size: head.size, tag: head.tag})
	if len(s.group) >= wire.Window {
		return s.commit()
	}
	return nil
}

// commit writes the entries of the group, and answers each put of it.
func (s *session) commit() error {
	group := s.group
	s.group = nil
	results, err := s.store.commit(s.account, group)
	if err != nil {
		return s.refuse(group[0].n, err)
	}
	for i, r := range results {
		var err error
		if r.err != nil {
			err = s.refuse(group[i].n, r.err)
		} else {
			err = s.reply(wire.Reply{N: group[i].n, Status: r.status, Seq: r.seq})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// refuse answers with the code of a refusal of the store. Another error
// is a failure of this machine; the reply does not show it.
func (s *session) refuse(n uint64, err error) error {
	if r, ok := errors.AsType[refusal](err); ok {
		return s.reply(wire.Reply{N: n, Error: r.code, Message: r.message})
	}
	_ = s.reply(wire.Reply{N: n, Error: wire.ErrFailed, Message: "the storage citizen could not do it"})
	return err
}

func (s *session) reply(r wire.Reply) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.dc.SendText(string(raw))
}
