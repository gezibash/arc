// Package ble provides an experimental BlueZ radio probe, not an ARC event
// transport. Its GATT service is deliberately separate from future secure ARC.
package ble

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	ServiceUUID = "8f71c100-5b9a-4cf0-a8d1-3e290da1b301"
	EchoUUID    = "8f71c101-5b9a-4cf0-a8d1-3e290da1b301"
	ProbeBytes  = 20 // fits the default ATT MTU (23 minus 3 write-header bytes)
	bluez       = "org.bluez"
	adapterIF   = bluez + ".Adapter1"
	deviceIF    = bluez + ".Device1"
	serviceIF   = bluez + ".GattService1"
	charIF      = bluez + ".GattCharacteristic1"
	managerIF   = "org.freedesktop.DBus.ObjectManager"
)

type objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant

type Options struct {
	Adapter string
	Peer    string // optional Bluetooth address; never an authenticated ARC identity
	Timeout time.Duration
	Output  io.Writer
}

func normalize(o Options) (Options, error) {
	if o.Adapter == "" {
		o.Adapter = "hci0"
	}
	if !regexp.MustCompile(`^hci[0-9]+$`).MatchString(o.Adapter) {
		return o, errors.New("adapter must be hci followed by a number")
	}
	if o.Timeout <= 0 {
		return o, errors.New("timeout must be positive")
	}
	if o.Output == nil {
		o.Output = io.Discard
	}
	if o.Peer != "" {
		mac, err := net.ParseMAC(o.Peer)
		if err != nil || len(mac) != 6 {
			return o, errors.New("peer must be a six-byte Bluetooth address")
		}
		o.Peer = strings.ToUpper(mac.String())
	}
	return o, nil
}

// radio makes lifecycle and cancellation behavior testable without a radio.
type radio interface {
	Call(context.Context, dbus.ObjectPath, string, ...any) error
	Objects(context.Context) (objects, error)
	Read(context.Context, dbus.ObjectPath) ([]byte, error)
}

func property[T any](p map[string]dbus.Variant, k string) T {
	v, _ := p[k].Value().(T)
	return v
}

func hasUUID(p map[string]dbus.Variant, uuid string) bool {
	for _, u := range property[[]string](p, "UUIDs") {
		if strings.EqualFold(u, uuid) {
			return true
		}
	}
	return false
}

func characteristic(all objects, device dbus.ObjectPath) (dbus.ObjectPath, error) {
	var found dbus.ObjectPath
	for path, interfaces := range all {
		c := interfaces[charIF]
		if !strings.EqualFold(property[string](c, "UUID"), EchoUUID) {
			continue
		}
		s := all[property[dbus.ObjectPath](c, "Service")][serviceIF]
		if property[dbus.ObjectPath](s, "Device") != device || !strings.EqualFold(property[string](s, "UUID"), ServiceUUID) {
			continue
		}
		flags := property[[]string](c, "Flags")
		read, write := false, false
		for _, flag := range flags {
			read = read || flag == "read"
			write = write || flag == "write"
		}
		if !read || !write {
			return "", errors.New("probe characteristic needs read and write support")
		}
		if found != "" {
			return "", errors.New("ambiguous probe characteristics")
		}
		found = path
	}
	if found == "" {
		return "", errors.New("probe characteristic not found")
	}
	return found, nil
}

func cleanup(r radio, path dbus.ObjectPath, method string, args ...any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return r.Call(ctx, path, method, args...)
}

func exchange(ctx context.Context, r radio, device dbus.ObjectPath) (err error) {
	all, err := r.Objects(ctx)
	if err != nil {
		return err
	}
	if property[bool](all[device][deviceIF], "Connected") {
		return errors.New("peer already connected; use an unused test connection")
	}
	// Disconnect even when Connect times out: BlueZ may finish after cancellation.
	defer func() { err = errors.Join(err, cleanup(r, device, deviceIF+".Disconnect")) }()
	if err = r.Call(ctx, device, deviceIF+".Connect"); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		all, err = r.Objects(ctx)
		if err != nil {
			return err
		}
		if property[bool](all[device][deviceIF], "ServicesResolved") {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	path, err := characteristic(all, device)
	if err != nil {
		return err
	}
	payload := make([]byte, ProbeBytes)
	if _, err = rand.Read(payload); err != nil {
		return err
	}
	if err = r.Call(ctx, path, charIF+".WriteValue", payload, map[string]dbus.Variant{"type": dbus.MakeVariant("request")}); err != nil {
		return err
	}
	reply, err := r.Read(ctx, path)
	if err != nil {
		return err
	}
	if !bytes.Equal(payload, reply) {
		return errors.New("Bluetooth probe reply did not match sent bytes")
	}
	return nil
}

func discover(ctx context.Context, r radio, o Options) (err error) {
	adapter := dbus.ObjectPath("/org/bluez/" + o.Adapter)
	filter := map[string]dbus.Variant{"Transport": dbus.MakeVariant("le"), "UUIDs": dbus.MakeVariant([]string{ServiceUUID})}
	if err = r.Call(ctx, adapter, adapterIF+".SetDiscoveryFilter", filter); err != nil {
		return err
	}
	if err = r.Call(ctx, adapter, adapterIF+".StartDiscovery"); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, cleanup(r, adapter, adapterIF+".StopDiscovery")) }()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	seen := make(map[dbus.ObjectPath]bool)
	for {
		all, e := r.Objects(ctx)
		if e != nil {
			if o.Peer == "" && errors.Is(e, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil
			}
			return e
		}
		for path, interfaces := range all {
			p := interfaces[deviceIF]
			if property[dbus.ObjectPath](p, "Adapter") != adapter || !hasUUID(p, ServiceUUID) {
				continue
			}
			address := strings.ToUpper(property[string](p, "Address"))
			if !seen[path] && len(seen) < 128 {
				fmt.Fprintf(o.Output, "peer %s\n", address)
				seen[path] = true
			}
			if o.Peer != "" && address == o.Peer {
				if e = exchange(ctx, r, path); e != nil {
					return e
				}
				fmt.Fprintf(o.Output, "PASS: %d bytes echoed by %s\n", ProbeBytes, address)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			if o.Peer == "" && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func advertiseAndRun(ctx context.Context, r radio, adapter dbus.ObjectPath, run func() error) (err error) {
	if err = r.Call(ctx, adapter, bluez+".GattManager1.RegisterApplication", appPath, map[string]dbus.Variant{}); err != nil {
		return fmt.Errorf("register GATT service: %w", err)
	}
	defer func() {
		err = errors.Join(err, cleanup(r, adapter, bluez+".GattManager1.UnregisterApplication", appPath))
	}()
	if err = r.Call(ctx, adapter, bluez+".LEAdvertisingManager1.RegisterAdvertisement", advertPath, map[string]dbus.Variant{}); err != nil {
		return fmt.Errorf("advertise probe: %w", err)
	}
	defer func() {
		err = errors.Join(err, cleanup(r, adapter, bluez+".LEAdvertisingManager1.UnregisterAdvertisement", advertPath))
	}()
	return run()
}
