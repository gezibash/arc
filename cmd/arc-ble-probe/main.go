// arc-ble-probe tests Linux Bluetooth discovery and an unauthenticated byte
// round trip. It does not send ARC events or expose capabilities.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gezibash/arc/delivery/transport/ble"
)

func main() {
	var o ble.Options
	flag.StringVar(&o.Adapter, "adapter", "hci0", "Linux Bluetooth adapter")
	flag.StringVar(&o.Peer, "peer", "", "Bluetooth address to connect to and test (default: discover only)")
	flag.DurationVar(&o.Timeout, "timeout", time.Minute, "total probe duration")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(2)
	}
	o.Output = os.Stdout
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := ble.Run(ctx, o); err != nil {
		fmt.Fprintln(os.Stderr, "arc-ble-probe:", err)
		os.Exit(1)
	}
}
