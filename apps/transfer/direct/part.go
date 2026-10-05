package direct

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/pion/webrtc/v4"
)

// ErrTooManyBytes says that the other end wrote more bytes than the size of
// the file. The receiving end does not write these bytes.
var ErrTooManyBytes = errors.New("the sender wrote more bytes than the link says")

// ReceivedFile is the path of a file that a citizen gave to this app with a
// put request: received/<key of the citizen>/<sha256> in the state
// directory. The caller checks first that the key and the SHA-256 are 64
// hex digits each, so the path cannot be out of the directory.
func ReceivedFile(state, from, sum string) string {
	return filepath.Join(state, "received", from, sum)
}

// HashFile returns the SHA-256 of a file, in hex.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// Part is a file that arrives in pieces. It writes <path>.part, and renames
// it to path when all the bytes have the SHA-256 of the file.
type Part struct {
	path, sha256 string
	size         int64
	file         *os.File
	sum          hash.Hash
	have         atomic.Int64
}

// OpenPart opens the part file of path to add bytes, and reads the bytes
// that it holds into the SHA-256. A part file larger than size is not a
// start of the file, and starts again.
func OpenPart(path, sum string, size int64) (*Part, error) {
	f, err := os.OpenFile(path+".part", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	p := &Part{path: path, sha256: sum, size: size, file: f, sum: sha256.New()}
	info, err := f.Stat()
	if err == nil && info.Size() > size {
		err = f.Truncate(0)
	}
	var n int64
	if err == nil {
		n, err = io.Copy(p.sum, f)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	p.have.Store(n)
	return p, nil
}

// Have is the number of bytes of the file that the part file holds.
func (p *Part) Have() int64 { return p.have.Load() }

// Write adds bytes to the part file. The size of the file is the limit: an
// end that writes more must not fill the disk.
func (p *Part) Write(b []byte) error {
	if p.have.Load()+int64(len(b)) > p.size {
		return ErrTooManyBytes
	}
	if _, err := p.file.Write(b); err != nil {
		return err
	}
	p.sum.Write(b)
	p.have.Add(int64(len(b)))
	return nil
}

// Finish checks the size and the SHA-256 of all the bytes, and renames the
// part file. If a check fails, it removes the part file.
func (p *Part) Finish() error {
	if err := p.file.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(p.sum.Sum(nil)); p.have.Load() != p.size || got != p.sha256 {
		_ = os.Remove(p.path + ".part")
		return fmt.Errorf("the bytes do not have the SHA-256 of the link (%d bytes, %s); the part file is removed", p.have.Load(), got)
	}
	return os.Rename(p.path+".part", p.path)
}

// Close keeps the part file, so that a later transfer gets only the rest.
func (p *Part) Close() { _ = p.file.Close() }

// Discard removes the part file.
func (p *Part) Discard() {
	_ = p.file.Close()
	_ = os.Remove(p.path + ".part")
}

// Send writes the bytes of the file from offset to size on the data
// channel, in messages of ChunkSize, and then the text "end". It waits when
// the buffer of the data channel is full.
func Send(dc *webrtc.DataChannel, path string, offset, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	low := make(chan struct{}, 1)
	dc.SetBufferedAmountLowThreshold(BufferLow)
	dc.OnBufferedAmountLow(func() {
		select {
		case low <- struct{}{}:
		default:
		}
	})
	closed := make(chan struct{})
	dc.OnClose(func() { close(closed) })

	block := make([]byte, ChunkSize)
	for sent := offset; sent < size; {
		n, err := io.ReadFull(f, block[:min(int64(ChunkSize), size-sent)])
		if err != nil {
			return err
		}
		if err := dc.Send(block[:n]); err != nil {
			return err
		}
		sent += int64(n)
		if dc.BufferedAmount() > BufferHigh {
			select {
			case <-low:
			case <-closed:
				return errors.New("the receiver closed the channel")
			}
		}
	}
	return dc.SendText("end")
}
