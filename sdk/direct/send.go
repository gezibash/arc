package direct

import (
	"errors"
	"io"
	"time"

	"github.com/pion/webrtc/v4"
)

// Send writes n bytes of r on the data channel, in messages of ChunkSize.
// It waits when the buffer of the data channel holds more than BufferHigh
// bytes, until it holds BufferLow bytes or less. It does not set the
// close handler of the data channel, because the caller owns it.
func Send(dc *webrtc.DataChannel, r io.Reader, n int64) error {
	low := make(chan struct{}, 1)
	dc.SetBufferedAmountLowThreshold(BufferLow)
	dc.OnBufferedAmountLow(func() {
		select {
		case low <- struct{}{}:
		default:
		}
	})

	block := make([]byte, ChunkSize)
	for sent := int64(0); sent < n; {
		m, err := io.ReadFull(r, block[:min(int64(ChunkSize), n-sent)])
		if err != nil {
			return err
		}
		if err := dc.Send(block[:m]); err != nil {
			return err
		}
		sent += int64(m)
		for dc.BufferedAmount() > BufferHigh {
			select {
			case <-low:
			case <-time.After(100 * time.Millisecond):
				if dc.ReadyState() != webrtc.DataChannelStateOpen {
					return errors.New("the receiver closed the channel")
				}
			}
		}
	}
	return nil
}
