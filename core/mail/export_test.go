package mail

import "time"

// SetNow sets the clock of the mail, for tests.
func SetNow(m *Mail, now func() time.Time) { m.now = now }
