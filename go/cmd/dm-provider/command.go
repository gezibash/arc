package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A command runs against the mailbox of the caller. Every body that the
// store holds is a sealed token, so the provider reads no message.

var (
	tokenTextPattern = regexp.MustCompile(`^sealed-v1:[A-Za-z0-9+/=]+$`)
	attachPattern    = regexp.MustCompile(`^attach:([A-Za-z0-9_-][A-Za-z0-9._-]*):(sealed-v1:[A-Za-z0-9+/=]+)$`)
	namePattern      = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*$`)
)

// event is one message that a citizen receives without asking.
type event struct {
	To    string
	Topic string
	Meta  map[string]any
	Body  string
}

// request is one command of one caller.
type request struct {
	from    string
	body    string
	hasBody bool
	args    []string
	options map[string]any
}

// answer is what a command returns.
type answer struct {
	reply  string
	events []event
}

// run reads one message and answers it.
func (s *server) run(from, message string) (*answer, error) {
	line, body, hasBody := splitMessage(message)
	args, options := parseLine(line)

	if !isPublicKey(from) {
		return nil, errorf("forbidden no caller key")
	}

	ctx := &request{from: from, body: body, hasBody: hasBody, args: args, options: options}
	return s.dispatch(ctx)
}

func (s *server) dispatch(ctx *request) (*answer, error) {
	if len(ctx.args) == 0 {
		return text(help()), nil
	}

	rest := ctx.args[1:]

	switch ctx.args[0] {
	case "send":
		// The first form names one recipient. The second names them in --to.
		if len(rest) == 1 {
			ctx.options["to"] = rest[0]
		}
		return s.send(ctx)
	case "quota":
		return s.quota(ctx)
	case "purge":
		return s.purgeCommand(ctx)
	case "fetch":
		if len(rest) != 2 {
			return nil, errorf("not_found")
		}
		return s.fetch(ctx, rest[0], rest[1])
	case "conversations":
		return s.conversations(ctx)
	case "inbox":
		return s.inbox(ctx)
	case "thread":
		if len(rest) != 1 {
			return nil, errorf("invalid_address")
		}
		return s.thread(ctx, rest[0])
	case "read":
		if len(rest) != 1 {
			return nil, errorf("not_found")
		}
		return s.read(ctx, rest[0])
	case "ack":
		return s.receiptEach(ctx, rest, "read", func(msg *message) bool { return msg.From != ctx.from })
	case "archive":
		return s.receiptEach(ctx, rest, "archived", func(*message) bool { return true })
	case "status":
		if len(rest) != 1 {
			return nil, errorf("not_found")
		}
		return s.status(ctx, rest[0])
	case "react":
		if len(rest) != 2 {
			return nil, errorf("invalid_reaction one to four characters, no separators")
		}
		return s.react(ctx, rest[0], rest[1])
	case "retract":
		if len(rest) != 1 {
			return nil, errorf("not_found")
		}
		return s.retract(ctx, rest[0])
	case "block", "unblock", "mute", "unmute":
		return s.listCommand(ctx, ctx.args[0], rest)
	case "blocked":
		return lines(s.store.blocked(ctx.from), "no blocked keys"), nil
	case "muted":
		return lines(s.store.muted(ctx.from), "no muted keys"), nil
	case "settings":
		return s.settingsCommand(ctx, rest)
	case "whoami":
		return text(ctx.from), nil
	case "help":
		return text(help()), nil
	default:
		return nil, errorf("unknown_command %s", ctx.args[0])
	}
}

// -- send -------------------------------------------------------------------

// send writes one message into the mailbox of every holder.
//
// The body holds one sealed token for each recipient in the order of --to,
// then one for the sender, then the attachment lines in the same order, one
// group for each attachment.
func (s *server) send(ctx *request) (*answer, error) {
	recipients, err := recipientsOf(ctx.options["to"])
	if err != nil {
		return nil, err
	}

	tokens, attachments, err := s.parseSendBody(ctx, len(recipients)+1)
	if err != nil {
		return nil, err
	}
	if err := s.noneBlocked(ctx, recipients); err != nil {
		return nil, err
	}
	if err := s.withinBudget(ctx, recipients, tokens, attachments); err != nil {
		return nil, err
	}

	id, err := newULID()
	if err != nil {
		return nil, err
	}

	holders := unique(append(append([]string{}, recipients...), ctx.from))
	replyTo, _ := option(ctx.options, "reply_to")
	now := timestamp()

	var meta []attach
	for _, one := range attachments {
		meta = append(meta, attach{Name: one.name, Bytes: len(one.tokens[0])})
	}

	// A send lands in every mailbox, with its receipts, or in none. A write
	// that fails takes back what landed, and rebuilds each counter.
	written := func() error {
		for _, holder := range holders {
			at := tokenIndex(holder, recipients)

			msg := &message{
				ID: id, From: ctx.from, To: recipients, T: now, Enc: "sealed-v1",
				Body: tokens[at], ReplyTo: replyTo, Attachments: meta,
			}

			bytes, err := s.store.putMessage(holder, msg, false)
			if err != nil {
				return err
			}

			for _, one := range attachments {
				blob, err := s.store.putBlob(holder, id, one.name, one.tokens[at], false)
				if err != nil {
					return err
				}
				bytes += blob
			}
			s.store.bumpUsage(holder, bytes)
		}
		return nil
	}()

	if written != nil {
		for _, holder := range holders {
			s.store.removeMessage(holder, id)
			s.store.recountUsage(holder)
		}
		return nil, written
	}

	for _, peer := range recipients {
		s.store.addReceipt(peer, id, "delivered", receipt{})
	}

	// Each recipient gets an event that carries its own sealed token.
	events := fanOut(holders, ctx.from, "dm.new", map[string]any{"id": id, "t": now},
		func(holder string) string { return tokens[tokenIndex(holder, recipients)] })

	return &answer{reply: "id: " + id, events: events}, nil
}

// attachment is one attachment, with one sealed token for each holder.
type attachment struct {
	name   string
	tokens []string
}

// parseSendBody reads the sealed tokens of a send.
func (s *server) parseSendBody(ctx *request, count int) ([]string, []attachment, error) {
	if !ctx.hasBody {
		return nil, nil, errorf("unsealed missing body")
	}

	var lines []string
	for _, line := range strings.Split(ctx.body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}

	if len(lines) < count {
		return nil, nil, errorf("unsealed body needs %d sealed-v1 tokens, one per line", count)
	}

	tokens := lines[:count]
	for _, token := range tokens {
		if err := s.sealedToken(token, s.config.MaxBody); err != nil {
			return nil, nil, err
		}
	}

	attachments, err := s.parseAttachments(lines[count:], count)
	if err != nil {
		return nil, nil, err
	}
	return tokens, attachments, nil
}

func (s *server) parseAttachments(lines []string, count int) ([]attachment, error) {
	if len(lines) == 0 {
		return nil, nil
	}

	parsed := make([][]string, 0, len(lines))
	for _, line := range lines {
		groups := attachPattern.FindStringSubmatch(line)
		if groups == nil {
			return nil, errorf("unsealed attachment lines must be attach:<name>:<sealed-v1 token>")
		}
		parsed = append(parsed, groups)
	}
	if len(parsed)%count != 0 {
		return nil, errorf("unsealed each attachment needs %d tokens", count)
	}

	var attachments []attachment
	for start := 0; start < len(parsed); start += count {
		group := parsed[start : start+count]

		name := group[0][1]
		tokens := make([]string, 0, count)
		for _, one := range group {
			if one[1] != name {
				return nil, errorf("unsealed attachment group mixes names")
			}
			if err := s.sealedToken(one[2], s.config.MaxAttach); err != nil {
				return nil, err
			}
			tokens = append(tokens, one[2])
		}
		attachments = append(attachments, attachment{name: name, tokens: tokens})
	}
	return attachments, nil
}

func (s *server) sealedToken(token string, cap int) error {
	switch {
	case !tokenTextPattern.MatchString(token):
		return errorf("unsealed")
	case len(token) > cap:
		return errorf("too_large max %d bytes", cap)
	default:
		return nil
	}
}

// withinBudget checks that the send fits in the mailbox of every holder.
func (s *server) withinBudget(ctx *request, recipients, tokens []string, attachments []attachment) error {
	for _, holder := range unique(append(append([]string{}, recipients...), ctx.from)) {
		at := tokenIndex(holder, recipients)

		size := len(tokens[at])
		for _, one := range attachments {
			size += len(one.tokens[at])
		}

		if s.store.usage(holder)+size > s.config.MailboxBudget {
			return errorf("too_large mailbox %s full", holder)
		}
	}
	return nil
}

func (s *server) noneBlocked(ctx *request, recipients []string) error {
	for _, peer := range recipients {
		if s.store.isBlocked(peer, ctx.from) {
			return errorf("blocked %s", peer)
		}
	}
	return nil
}

// tokenIndex is where the token of one holder lies. The token of the sender
// is last. A sender who is also a recipient keeps the recipient token, and
// either one opens for them.
func tokenIndex(holder string, recipients []string) int {
	for index, peer := range recipients {
		if peer == holder {
			return index
		}
	}
	return len(recipients)
}

// -- mailbox ----------------------------------------------------------------

func (s *server) quota(ctx *request) (*answer, error) {
	return text(fmt.Sprintf("used %d of %d bytes", s.store.usage(ctx.from), s.config.MailboxBudget)), nil
}

func (s *server) purgeCommand(ctx *request) (*answer, error) {
	before, ok := option(ctx.options, "before")
	if !ok {
		return nil, errorf("missing --before <id>")
	}
	if !validULID(before) {
		return nil, errNotFound
	}
	return text(fmt.Sprintf("purged %d", s.store.purge(ctx.from, before))), nil
}

func (s *server) fetch(ctx *request, id, name string) (*answer, error) {
	if !validULID(id) || !namePattern.MatchString(name) {
		return nil, errNotFound
	}
	if _, err := s.store.getMessage(ctx.from, id); err != nil {
		return nil, err
	}

	token, err := s.store.getBlob(ctx.from, id, name)
	if err != nil {
		return nil, err
	}
	return text(token), nil
}

// -- conversations ----------------------------------------------------------

func (s *server) conversations(ctx *request) (*answer, error) {
	index := s.store.receiptIndex(ctx.from)
	muted := s.store.muted(ctx.from)

	grouped := map[string][]*message{}
	var order []string
	for _, msg := range s.store.listMessages(ctx.from) {
		peer := other(msg, ctx.from)
		if _, held := grouped[peer]; !held {
			order = append(order, peer)
		}
		grouped[peer] = append(grouped[peer], msg)
	}

	type conversation struct {
		peer   string
		last   *message
		unread int
		muted  bool
	}

	found := make([]conversation, 0, len(order))
	for _, peer := range order {
		messages := grouped[peer]
		last := messages[len(messages)-1]
		isMuted := contains(muted, peer)

		unread := 0
		if !isMuted {
			for _, msg := range messages {
				if msg.From != ctx.from && !hasEvent(index, msg.ID, "read") {
					unread++
				}
			}
		}
		found = append(found, conversation{peer: peer, last: last, unread: unread, muted: isMuted})
	}

	sort.Slice(found, func(left, right int) bool {
		return found[left].last.ID > found[right].last.ID
	})
	found = found[:limitFirst(len(found), ctx.options)]

	total := 0
	for _, one := range found {
		total += one.unread
	}

	rows := []string{fmt.Sprintf("%d unread in %d conversations", total, len(found))}
	for _, one := range found {
		flag := "-"
		if one.muted {
			flag = "muted"
		}
		rows = append(rows, strings.Join([]string{
			one.peer, one.last.ID, one.last.T, strconv.Itoa(one.unread), flag, one.last.Body,
		}, "\t"))
	}
	return text(strings.Join(rows, "\n")), nil
}

// -- inbox and thread -------------------------------------------------------

func (s *server) inbox(ctx *request) (*answer, error) {
	index := s.store.receiptIndex(ctx.from)
	muted := s.store.muted(ctx.from)
	onlyUnread := flagSet(ctx.options, "unread")

	var found []*message
	for _, msg := range s.store.listMessages(ctx.from) {
		if hasEvent(index, msg.ID, "archived") {
			continue
		}
		if onlyUnread {
			read := msg.From == ctx.from || hasEvent(index, msg.ID, "read")
			if read || contains(muted, other(msg, ctx.from)) {
				continue
			}
		}
		found = append(found, msg)
	}

	found = since(found, ctx.options)
	found = lastOf(found, ctx.options)

	rows := make([]string, 0, len(found))
	for _, msg := range found {
		rows = append(rows, line(msg, ctx.from))
	}
	return lines(rows, "no messages"), nil
}

// thread shows one conversation. The peer is a conversation key as
// conversations writes it: one key, or a set joined by commas.
func (s *server) thread(ctx *request, peer string) (*answer, error) {
	key, err := conversationKey(peer)
	if err != nil {
		return nil, err
	}

	var found []*message
	for _, msg := range s.store.listMessages(ctx.from) {
		if other(msg, ctx.from) == key {
			found = append(found, msg)
		}
	}
	found = lastOf(since(found, ctx.options), ctx.options)

	if flagSet(ctx.options, "bodies") {
		return s.threadWithBodies(ctx, found, key)
	}

	rows := make([]string, 0, len(found))
	for _, msg := range found {
		rows = append(rows, line(msg, ctx.from))
	}
	return lines(rows, "no messages"), nil
}

// threadWithBodies writes every message with its body, and marks the
// messages that arrived as read.
func (s *server) threadWithBodies(ctx *request, messages []*message, peer string) (*answer, error) {
	receipts := s.store.allReceipts(ctx.from)
	index := receiptIndex(receipts)
	reactions := reactionIndex(receipts)

	// A message that went out is read once every peer who shares receipts
	// has read it. A peer with receipts off never counts.
	var peerIndexes []map[string]map[string]bool
	for _, one := range strings.Split(peer, ",") {
		if s.store.settings(one)["receipts"] == "on" {
			peerIndexes = append(peerIndexes, s.store.receiptIndex(one))
		}
	}

	peersRead := func(msg *message) bool {
		if len(peerIndexes) == 0 {
			return false
		}
		for _, held := range peerIndexes {
			if !hasEvent(held, msg.ID, "read") {
				return false
			}
		}
		return true
	}

	var unread []*message
	rows := make([]string, 0, len(messages))

	for _, msg := range messages {
		outbound := msg.From == ctx.from
		if !outbound && !hasEvent(index, msg.ID, "read") {
			unread = append(unread, msg)
		}

		state := "unread"
		switch {
		case hasEvent(index, msg.ID, "retracted"):
			state = "retracted"
		case outbound && peersRead(msg):
			state = "read"
		case outbound:
			state = "delivered"
		case hasEvent(index, msg.ID, "read"):
			state = "read"
		}

		direction := "in"
		who := msg.From
		if outbound {
			direction = "out"
			who = strings.Join(msg.To, ",")
		}

		replyTo := msg.ReplyTo
		if replyTo == "" {
			replyTo = "-"
		}

		rows = append(rows, strings.Join([]string{
			msg.ID, direction, who, msg.T, replyTo,
			flagsField(state, reactions[msg.ID], msg.Attachments), msg.Body,
		}, "\t"))
	}

	for _, msg := range unread {
		s.store.addReceipt(ctx.from, msg.ID, "read", receipt{})
	}

	header := fmt.Sprintf("%s · %d messages, %d unread", peer, len(messages), len(unread))
	return text(strings.Join(append([]string{header}, rows...), "\n")), nil
}

// -- read, receipts and state ------------------------------------------------

func (s *server) read(ctx *request, id string) (*answer, error) {
	if !validULID(id) {
		return nil, errNotFound
	}

	msg, err := s.store.getMessage(ctx.from, id)
	if err != nil {
		return nil, err
	}

	receipts := s.store.allReceipts(ctx.from)
	index := receiptIndex(receipts)

	if msg.From != ctx.from && !hasEvent(index, id, "read") {
		s.store.addReceipt(ctx.from, id, "read", receipt{})
	}

	return text(render(msg, hasEvent(index, id, "retracted"), reactionIndex(receipts)[id])), nil
}

func (s *server) receiptEach(ctx *request, ids []string, event string, allowed func(*message) bool) (*answer, error) {
	if len(ids) == 0 {
		return nil, errorf("unknown_command %s", event)
	}

	for _, id := range ids {
		if !validULID(id) {
			return nil, errNotFound
		}

		msg, err := s.store.getMessage(ctx.from, id)
		if err != nil {
			return nil, err
		}
		if !allowed(msg) {
			return nil, errorf("forbidden %s", id)
		}
		s.store.addReceipt(ctx.from, id, event, receipt{})
	}

	if event == "read" {
		return text(fmt.Sprintf("acked %d", len(ids))), nil
	}
	return text(fmt.Sprintf("archived %d", len(ids))), nil
}

func (s *server) status(ctx *request, id string) (*answer, error) {
	if !validULID(id) {
		return nil, errNotFound
	}

	msg, err := s.store.getMessage(ctx.from, id)
	if err != nil {
		return nil, err
	}
	if msg.From != ctx.from {
		return nil, errorf("forbidden not the sender")
	}

	var rows []string
	for _, peer := range msg.To {
		// Each recipient decides whether a sender sees the read receipts.
		showRead := s.store.settings(peer)["receipts"] == "on"

		for _, one := range s.store.receiptsOf(peer, id) {
			if one.Event == "delivered" || (showRead && one.Event == "read") {
				rows = append(rows, fmt.Sprintf("%s %s %s", one.Event, one.T, peer))
			}
		}
	}
	return lines(rows, "no receipts"), nil
}

func (s *server) react(ctx *request, id, value string) (*answer, error) {
	if !validULID(id) {
		return nil, errNotFound
	}
	if err := validReaction(value); err != nil {
		return nil, err
	}

	msg, err := s.store.getMessage(ctx.from, id)
	if err != nil {
		return nil, err
	}

	if value == "none" {
		value = ""
	}

	for _, holder := range holdersOf(msg) {
		s.store.addReceipt(holder, id, "reaction", receipt{By: ctx.from, Value: value})
	}

	events := fanOut(holdersOf(msg), ctx.from, "dm.reaction",
		map[string]any{"id": id, "value": value}, nil)

	reply := fmt.Sprintf("reacted %s %s", value, id)
	if value == "" {
		reply = "cleared " + id
	}
	return &answer{reply: reply, events: events}, nil
}

func (s *server) retract(ctx *request, id string) (*answer, error) {
	if !validULID(id) {
		return nil, errNotFound
	}

	msg, err := s.store.getMessage(ctx.from, id)
	if err != nil {
		return nil, err
	}
	if msg.From != ctx.from {
		return nil, errorf("forbidden not the sender")
	}
	if !s.withinRetractWindow(msg) {
		return nil, errorf("too_late")
	}

	// A recipient who purged their copy has nothing to blank. Every step
	// here runs again without harm, so a retry after a failure finishes.
	for _, holder := range holdersOf(msg) {
		if holder == ctx.from {
			continue
		}
		copy, err := s.store.getMessage(holder, id)
		if err != nil {
			continue
		}
		copy.Body = ""
		s.store.putMessage(holder, copy, true)
	}

	for _, holder := range holdersOf(msg) {
		s.store.addReceipt(holder, id, "retracted", receipt{})
	}

	// The dm.new event already carried the body to a recipient who watched.
	// The dm.retracted event tells that watcher to drop it.
	events := fanOut(holdersOf(msg), ctx.from, "dm.retracted", map[string]any{"id": id}, nil)
	return &answer{reply: "retracted " + id, events: events}, nil
}

func (s *server) withinRetractWindow(msg *message) bool {
	sent, err := time.Parse(time.RFC3339, msg.T)
	if err != nil {
		return false
	}
	return int(time.Since(sent).Seconds()) <= s.config.RetractWindow
}

// -- lists and settings ------------------------------------------------------

func (s *server) listCommand(ctx *request, name string, rest []string) (*answer, error) {
	if len(rest) != 1 || !isPublicKey(rest[0]) {
		return nil, errorf("invalid_address")
	}
	peer := rest[0]

	read, write := s.store.blocked, s.store.writeBlocked
	if name == "mute" || name == "unmute" {
		read, write = s.store.muted, s.store.writeMuted
	}

	keys := read(ctx.from)
	if strings.HasPrefix(name, "un") {
		keys = without(keys, peer)
	} else {
		keys = unique(append(keys, peer))
	}

	if err := write(ctx.from, keys); err != nil {
		return nil, err
	}
	return text(fmt.Sprintf("%sed %s", name, peer)), nil
}

func (s *server) settingsCommand(ctx *request, rest []string) (*answer, error) {
	if len(rest) == 0 {
		held := s.store.settings(ctx.from)

		names := make([]string, 0, len(held))
		for name := range held {
			names = append(names, name)
		}
		sort.Strings(names)

		rows := make([]string, 0, len(names))
		for _, name := range names {
			rows = append(rows, name+" "+held[name])
		}
		return lines(rows, "no settings"), nil
	}

	if rest[0] != "receipts" || len(rest) != 2 || (rest[1] != "on" && rest[1] != "off") {
		return nil, errorf("invalid_setting %s: receipts on|off", rest[0])
	}

	held := s.store.settings(ctx.from)
	held["receipts"] = rest[1]
	if err := s.store.writeSettings(ctx.from, held); err != nil {
		return nil, err
	}
	return text("receipts " + rest[1]), nil
}
