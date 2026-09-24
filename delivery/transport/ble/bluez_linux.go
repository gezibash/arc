package ble

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

type busRadio struct{ conn *dbus.Conn }

func (r busRadio) Call(ctx context.Context, path dbus.ObjectPath, method string, args ...any) error {
	return r.conn.Object(bluez, path).CallWithContext(ctx, method, 0, args...).Err
}
func (r busRadio) Objects(ctx context.Context) (objects, error) {
	var all objects
	err := r.conn.Object(bluez, "/").CallWithContext(ctx, managerIF+".GetManagedObjects", 0).Store(&all)
	return all, err
}
func (r busRadio) Read(ctx context.Context, path dbus.ObjectPath) ([]byte, error) {
	var value []byte
	err := r.conn.Object(bluez, path).CallWithContext(ctx, charIF+".ReadValue", 0, map[string]dbus.Variant{}).Store(&value)
	return value, err
}

// Run advertises the probe service while scanning. With Peer set it also
// connects, writes a random challenge and checks the echoed value. It never
// changes adapter power, pairing, identity or ARC relay configuration.
func Run(ctx context.Context, o Options) (err error) {
	o, err = normalize(o)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("connect to system D-Bus: %w", err)
	}
	defer conn.Close()
	r := busRadio{conn}
	all, err := r.Objects(ctx)
	if err != nil {
		return fmt.Errorf("query BlueZ: %w", err)
	}
	adapter := dbus.ObjectPath("/org/bluez/" + o.Adapter)
	interfaces := all[adapter]
	if !property[bool](interfaces[adapterIF], "Powered") {
		return errors.New("Bluetooth adapter missing or powered off; check bluetoothctl show")
	}
	for _, name := range []string{bluez + ".GattManager1", bluez + ".LEAdvertisingManager1"} {
		if _, ok := interfaces[name]; !ok {
			return fmt.Errorf("adapter lacks %s", name)
		}
	}
	var owner string
	if err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, bluez).Store(&owner); err != nil {
		return err
	}
	server := &echoServer{values: make(map[dbus.ObjectPath]sample), now: time.Now, owner: owner}
	managed := serviceObjects()
	if err = conn.Export(&objectManager{managed}, appPath, managerIF); err != nil {
		return err
	}
	if err = conn.Export(server, echoPath, charIF); err != nil {
		return err
	}
	for path, ifaces := range managed {
		props := prop.Map{}
		for iface, values := range ifaces {
			props[iface] = map[string]*prop.Prop{}
			for name, value := range values {
				props[iface][name] = &prop.Prop{Value: value.Value(), Writable: false, Emit: prop.EmitFalse}
			}
		}
		if _, err = prop.Export(conn, path, props); err != nil {
			return err
		}
	}
	if err = conn.Export(&advertisement{}, advertPath, advertIF); err != nil {
		return err
	}
	if _, err = prop.Export(conn, advertPath, prop.Map{advertIF: {
		"Type": {Value: "peripheral"}, "ServiceUUIDs": {Value: []string{ServiceUUID}},
	}}); err != nil {
		return err
	}
	return advertiseAndRun(ctx, r, adapter, func() error {
		fmt.Fprintf(o.Output, "advertising ARC probe on %s (%s); scanning\n", o.Adapter, property[string](interfaces[adapterIF], "Address"))
		return discover(ctx, r, o)
	})
}
