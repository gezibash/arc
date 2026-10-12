package ble

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

const directServiceUUID = "8f71c110-5b9a-4cf0-a8d1-3e290da1b301"
const directDataUUID = "8f71c111-5b9a-4cf0-a8d1-3e290da1b301"
const directApp dbus.ObjectPath = "/org/arc/direct"
const directService dbus.ObjectPath = directApp + "/service"
const directData dbus.ObjectPath = directService + "/data"
const directAdvert dbus.ObjectPath = "/org/arc/direct_advertisement"

// OpenDirect opens one Linux BLE link to an explicitly expected ARC identity.
// It returns only after Noise and ARC identity authentication. The caller's
// context owns the connection lifetime. Connection setup has a 30-second cap.
func OpenDirect(ctx context.Context, c DirectConfig) (*Direct, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o, err := normalize(Options{Adapter: c.Adapter, Peer: c.Address, Timeout: operationTimeout})
	if err != nil {
		return nil, err
	}
	if c.Signer == nil || c.Store == nil || c.Share == nil {
		return nil, errors.New("bluetooth: signer, store and share policy are required")
	}
	setup, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}
	lctx, stop := context.WithCancel(ctx)
	l := &gattLink{ctx: lctx, cancel: stop, r: busRadio{conn}, adapter: dbus.ObjectPath("/org/bluez/" + o.Adapter), in: make(chan []byte, 256), out: make(chan []byte, 256), ready: make(chan struct{})}
	ok := false
	defer func() {
		if !ok {
			_ = l.Close()
		}
	}()
	all, err := l.r.Objects(setup)
	if err != nil {
		return nil, err
	}
	if !property[bool](all[l.adapter][adapterIF], "Powered") {
		return nil, errors.New("bluetooth: adapter missing or powered off")
	}
	if o.Peer != "" {
		err = l.connect(setup, o.Peer)
	} else {
		err = l.listen(setup)
	}
	if err != nil {
		return nil, err
	}
	go l.monitor()
	secure, err := authenticate(setup, l, c, o.Peer != "")
	if err != nil {
		return nil, err
	}
	d := newDirect(ctx, secure, c)
	ok = true
	return d, nil
}

type gattLink struct {
	ctx                              context.Context
	cancel                           context.CancelFunc
	r                                busRadio
	adapter                          dbus.ObjectPath
	device, characteristic           dbus.ObjectPath
	owner                            string
	mu                               sync.Mutex
	in, out                          chan []byte
	ready                            chan struct{}
	peripheral                       bool
	registered, advertised, scanning bool
	once                             sync.Once
}

func (l *gattLink) Close() error {
	l.once.Do(func() {
		l.cancel()
		l.mu.Lock()
		device := l.device
		l.mu.Unlock()
		if device != "" {
			_ = cleanup(l.r, device, deviceIF+".Disconnect")
		}
		if l.scanning {
			_ = cleanup(l.r, l.adapter, adapterIF+".StopDiscovery")
		}
		if l.advertised {
			_ = cleanup(l.r, l.adapter, bluez+".LEAdvertisingManager1.UnregisterAdvertisement", directAdvert)
		}
		if l.registered {
			_ = cleanup(l.r, l.adapter, bluez+".GattManager1.UnregisterApplication", directApp)
		}
		_ = l.r.conn.Close()
	})
	return nil
}
func (l *gattLink) monitor() {
	defer func() { _ = l.Close() }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(l.ctx, 3*time.Second)
			all, err := l.r.Objects(ctx)
			cancel()
			l.mu.Lock()
			device := l.device
			l.mu.Unlock()
			if err != nil || !property[bool](all[device][deviceIF], "Connected") {
				l.cancel()
				return
			}
		}
	}
}
func (l *gattLink) write(ctx context.Context, b []byte) error {
	if len(b) == 0 || len(b) > 20 {
		return errors.New("bluetooth: invalid packet length")
	}
	if l.peripheral {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-l.ctx.Done():
			return ErrDisconnected
		case l.out <- append([]byte(nil), b...):
			return nil
		}
	}
	return l.r.Call(ctx, l.characteristic, charIF+".WriteValue", b, map[string]dbus.Variant{"type": dbus.MakeVariant("request")})
}
func (l *gattLink) read(ctx context.Context) ([]byte, error) {
	if l.peripheral {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-l.ctx.Done():
			return nil, ErrDisconnected
		case b := <-l.in:
			return b, nil
		}
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		b, err := l.r.Read(readCtx, l.characteristic)
		cancel()
		if err != nil {
			return nil, err
		}
		if len(b) > 0 {
			return b, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-l.ctx.Done():
			return nil, ErrDisconnected
		case <-ticker.C:
		}
	}
}
func (l *gattLink) connect(ctx context.Context, address string) error {
	filter := map[string]dbus.Variant{"Transport": dbus.MakeVariant("le"), "UUIDs": dbus.MakeVariant([]string{directServiceUUID})}
	if err := l.r.Call(ctx, l.adapter, adapterIF+".SetDiscoveryFilter", filter); err != nil {
		return err
	}
	if err := l.r.Call(ctx, l.adapter, adapterIF+".StartDiscovery"); err != nil {
		return err
	}
	l.scanning = true
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		all, err := l.r.Objects(ctx)
		if err != nil {
			return err
		}
		for path, ifs := range all {
			p := ifs[deviceIF]
			if property[dbus.ObjectPath](p, "Adapter") != l.adapter || !strings.EqualFold(property[string](p, "Address"), address) || !hasUUID(p, directServiceUUID) {
				continue
			}
			if property[bool](p, "Connected") {
				return errors.New("bluetooth: peer already connected")
			}
			l.device = path
			if err = l.r.Call(ctx, path, deviceIF+".Connect"); err != nil {
				return err
			}
			return l.resolve(ctx)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (l *gattLink) resolve(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		all, err := l.r.Objects(ctx)
		if err != nil {
			return err
		}
		if property[bool](all[l.device][deviceIF], "ServicesResolved") {
			for path, ifs := range all {
				p := ifs[charIF]
				s := all[property[dbus.ObjectPath](p, "Service")][serviceIF]
				if strings.EqualFold(property[string](p, "UUID"), directDataUUID) && strings.EqualFold(property[string](s, "UUID"), directServiceUUID) && property[dbus.ObjectPath](s, "Device") == l.device {
					if l.characteristic != "" {
						return errors.New("bluetooth: ambiguous direct service")
					}
					l.characteristic = path
				}
			}
			if l.characteristic == "" {
				return errors.New("bluetooth: direct characteristic missing")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (l *gattLink) listen(ctx context.Context) error {
	l.peripheral = true
	if err := l.r.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, bluez).Store(&l.owner); err != nil {
		return err
	}
	managed := objects{
		directService: {serviceIF: {"UUID": dbus.MakeVariant(directServiceUUID), "Primary": dbus.MakeVariant(true)}},
		directData:    {charIF: {"UUID": dbus.MakeVariant(directDataUUID), "Service": dbus.MakeVariant(directService), "Flags": dbus.MakeVariant([]string{"read", "write"})}},
	}
	if err := l.r.conn.Export(&objectManager{managed}, directApp, managerIF); err != nil {
		return err
	}
	if err := l.r.conn.Export(&directCharacteristic{l}, directData, charIF); err != nil {
		return err
	}
	for path, ifs := range managed {
		props := prop.Map{}
		for iface, values := range ifs {
			props[iface] = map[string]*prop.Prop{}
			for name, value := range values {
				props[iface][name] = &prop.Prop{Value: value.Value(), Writable: false, Emit: prop.EmitFalse}
			}
		}
		if _, err := prop.Export(l.r.conn, path, props); err != nil {
			return err
		}
	}
	if err := l.r.conn.Export(&advertisement{}, directAdvert, advertIF); err != nil {
		return err
	}
	if _, err := prop.Export(l.r.conn, directAdvert, prop.Map{advertIF: {"Type": {Value: "peripheral"}, "ServiceUUIDs": {Value: []string{directServiceUUID}}}}); err != nil {
		return err
	}
	if err := l.r.Call(ctx, l.adapter, bluez+".GattManager1.RegisterApplication", directApp, map[string]dbus.Variant{}); err != nil {
		return err
	}
	l.registered = true
	if err := l.r.Call(ctx, l.adapter, bluez+".LEAdvertisingManager1.RegisterAdvertisement", directAdvert, map[string]dbus.Variant{}); err != nil {
		return err
	}
	l.advertised = true
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.ready:
		return nil
	}
}

type directCharacteristic struct{ l *gattLink }

func (c *directCharacteristic) peer(sender dbus.Sender, opts map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	validator := echoServer{owner: c.l.owner}
	p, e := validator.device(sender, opts)
	if e != nil {
		return "", e
	}
	if !strings.HasPrefix(string(p), string(c.l.adapter)+"/dev_") {
		return "", fail("NotAuthorized")
	}
	return p, nil
}
func (c *directCharacteristic) WriteValue(sender dbus.Sender, value []byte, opts map[string]dbus.Variant) *dbus.Error {
	p, e := c.peer(sender, opts)
	if e != nil {
		return e
	}
	if len(value) == 0 || len(value) > 20 {
		return fail("InvalidValueLength")
	}
	c.l.mu.Lock()
	defer c.l.mu.Unlock()
	if c.l.ctx.Err() != nil {
		return fail("Failed")
	}
	if c.l.device == "" {
		c.l.device = p
		close(c.l.ready)
	} else if c.l.device != p {
		return fail("NotAuthorized")
	}
	select {
	case c.l.in <- append([]byte(nil), value...):
		return nil
	default:
		c.l.cancel()
		return fail("InProgress")
	}
}
func (c *directCharacteristic) ReadValue(sender dbus.Sender, opts map[string]dbus.Variant) ([]byte, *dbus.Error) {
	p, e := c.peer(sender, opts)
	if e != nil {
		return nil, e
	}
	c.l.mu.Lock()
	defer c.l.mu.Unlock()
	if p != c.l.device || c.l.ctx.Err() != nil {
		return nil, fail("NotAuthorized")
	}
	select {
	case b := <-c.l.out:
		return b, nil
	default:
		return []byte{}, nil
	}
}
