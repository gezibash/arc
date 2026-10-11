package direct

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/transport/v5/vnet"
	"github.com/pion/webrtc/v4"
)

// TestEmulatedLinkSpeed sends a file between two ends on an emulated link
// and prints the speed. It is a measurement, not a check: it fails only if
// the transfer fails. It runs only if DIRECT_LINK_MIB is set.
//
// The link has a rate limit and a drop-tail queue in the direction of the
// data, and a fixed delay in each direction. The variables of the link are
// DIRECT_LINK_MIB (the file size), DIRECT_LINK_MBIT (default 92.5),
// DIRECT_LINK_RTT_MS (default 44) and DIRECT_LINK_QUEUE_KIB
// (default 128). DIRECT_LINK_CA_STEP_KIB replaces sctpCAStep; 0 gives the
// default of pion.
func TestEmulatedLinkSpeed(t *testing.T) {
	mib := envFloat(t, "DIRECT_LINK_MIB", 0)
	if mib == 0 {
		t.Skip("set DIRECT_LINK_MIB to measure an emulated link")
	}
	mbit := envFloat(t, "DIRECT_LINK_MBIT", 92.5)
	rtt := time.Duration(envFloat(t, "DIRECT_LINK_RTT_MS", 44) * float64(time.Millisecond))
	queue := int64(envFloat(t, "DIRECT_LINK_QUEUE_KIB", 128) * 1024)
	if os.Getenv("DIRECT_LINK_CA_STEP_KIB") != "" {
		step := sctpCAStep
		sctpCAStep = uint32(envFloat(t, "DIRECT_LINK_CA_STEP_KIB", 0) * 1024)
		t.Cleanup(func() { sctpCAStep = step })
	}
	router, err := vnet.NewRouter(&vnet.RouterConfig{CIDR: "10.0.0.0/24", MinDelay: rtt / 2, LoggerFactory: logging.NewDefaultLoggerFactory()})
	if err != nil {
		t.Fatal(err)
	}
	senderNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"10.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	receiverNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"10.0.0.2"}})
	if err != nil {
		t.Fatal(err)
	}
	limited, err := vnet.NewQueue(receiverNet, vnet.NewTBFQueue(int(mbit*1e6), 16<<10, queue))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = limited.Close() })
	if err := router.AddNet(senderNet); err != nil {
		t.Fatal(err)
	}
	if err := router.AddNet(limited); err != nil {
		t.Fatal(err)
	}
	if err := router.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Stop() })

	size := int64(mib * (1 << 20))

	sender, err := NewConnection(Options{net: senderNet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	receiver, err := NewConnection(Options{net: receiverNet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receiver.Close() })

	done := make(chan int64, 1)
	receiver.OnDataChannel(func(dc *webrtc.DataChannel) {
		var got int64
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if m.IsString {
				done <- got
				return
			}
			got += int64(len(m.Data))
		})
	})
	dc, err := sender.CreateDataChannel("file", nil)
	if err != nil {
		t.Fatal(err)
	}
	var start time.Time
	sent := make(chan error, 1)
	dc.OnOpen(func() {
		start = time.Now()
		go func() {
			err := Send(dc, bytes.NewReader(make([]byte, size)), size)
			if err == nil {
				err = dc.SendText("end")
			}
			sent <- err
		}()
	})

	offer, err := sender.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if offer, err = Describe(sender, offer); err != nil {
		t.Fatal(err)
	}
	if err := receiver.SetRemoteDescription(offer); err != nil {
		t.Fatal(err)
	}
	answer, err := receiver.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer, err = Describe(receiver, answer); err != nil {
		t.Fatal(err)
	}
	if err := sender.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-sent:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Minute):
		t.Fatal("the sender did not finish")
	}
	got := <-done
	seconds := time.Since(start).Seconds()
	if got != size {
		t.Fatalf("received %d bytes, want %d", got, size)
	}
	fmt.Printf("link %.1f Mbit/s, RTT %v, queue %d KiB, CA step %d bytes: %d bytes in %.1f s, %.2f MiB/s, %.1f Mbit/s\n",
		mbit, rtt, queue>>10, sctpCAStep,
		got, seconds, float64(got)/seconds/(1<<20), float64(got)*8/seconds/1e6)
}

func envFloat(t *testing.T, name string, fallback float64) float64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return f
}
