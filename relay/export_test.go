package relay

import "net"

// LinkTo returns the connection of the ready link to one partner, or nil.
func (r *Relay) LinkTo(peer []byte) net.Conn {
	r.federation.mu.Lock()
	defer r.federation.mu.Unlock()

	held := r.federation.links[string(peer)]
	if held == nil || !held.isReady() {
		return nil
	}
	return held.conn
}

// AnswerLookup answers one lookup as if a partner sent it.
func (r *Relay) AnswerLookup(peer []byte, request map[string]any) map[string]any {
	return r.answerLookup(peer, request)
}
