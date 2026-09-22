package citizen

import "github.com/gezibash/arc/direct"

// HoldCarrier puts a carrier in place for one peer, as a route that stands
// would.
func (c *Citizen) HoldCarrier(peer []byte, carrier *direct.Conn) {
	c.mu.Lock()
	c.carriers[string(peer)] = carrier
	c.mu.Unlock()
}

// SendControl sends one control message of a direct route.
func (c *Citizen) SendControl(peer, body []byte) error { return c.sendControl(peer, body) }

// Admit takes one request into the set that waits for the provider.
func (c *Citizen) Admit(peer, requestID []byte) (code, reason string) {
	return c.admit(peer, requestID)
}
