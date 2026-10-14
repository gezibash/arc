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
const propertiesIF = "org.freedesktop.DBus.Properties"

// confirmTimeout bounds the wait for the central to confirm one indication.
const confirmTimeout = 10 * time.Second

// setupRetry is the pause before a central tries a new connection.
const setupRetry = 500 * time.Millisecond

// OpenDirect opens one Linux BLE link to an explicitly expected ARC identity.
// It returns only after Noise and ARC identity authentication. The caller's
// context owns the connection lifetime. Connection setup has a 30-second cap.
// Within it, a central tries a new connection after a radio failure, because
// no ARC data has moved yet. It never retries a wrong identity.
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
	for {
		d, err := openOnce(ctx, setup, c, o)
		if err == nil || o.Peer == "" || !retryable(err) {
			return d, err
		}
		select {
		case <-setup.Done():
			return nil, err
		case <-time.After(setupRetry):
		}
	}
}

func openOnce(ctx, setup context.Context, c DirectConfig, o Options) (*Direct, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}
	lctx, stop := context.WithCancel(ctx)
	l := &gattLink{ctx: lctx, cancel: stop, r: busRadio{conn}, adapter: dbus.ObjectPath("/org/bluez/" + o.Adapter),
		in: make(chan []byte, 256), out: make(chan []byte, 256), ready: make(chan struct{}),
		subscribed: make(chan struct{}), confirm: make(chan struct{}, 1)}
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
	if err == nil {
		err = l.watch()
	}
	if err == nil && o.Peer != "" {
		err = l.subscribe(setup)
	}
	if err != nil {
		return nil, linkError{err}
	}
	go l.monitor()
	secure, err := authenticate(setup, l, c, o.Peer != "")
	if err != nil {
		if errors.Is(err, errIdentity) {
			return nil, err
		}
		return nil, linkError{err}
	}
	d := newDirect(ctx, secure, c)
	ok = true
	return d, nil
}

// gattLink carries packets over one GATT characteristic. The central writes
// with acknowledged requests. The peripheral answers with indications, which
// the central confirms one at a time, so neither side loses or floods packets.
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
	subscribed                       chan struct{} // peripheral: the central enabled indications
	confirm                          chan struct{} // peripheral: the central confirmed one indication
	size                             int           // packet size from the negotiated MTU; 0 until known
	peripheral                       bool
	registered, advertised, scanning bool
	notifying                        bool
	once, subscribeOnce              sync.Once
}

func (l *gattLink) payload() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size == 0 {
		return minPacket
	}
	return l.size
}

func (l *gattLink) Close() error {
	l.once.Do(func() {
		l.cancel()
		l.mu.Lock()
		device := l.device
		l.mu.Unlock()
		if l.notifying {
			_ = cleanup(l.r, l.characteristic, charIF+".StopNotify")
		}
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

// monitor polls the connection as a fallback for a lost signal.
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

// watch takes BlueZ signals for the device and, on a central, for the
// characteristic. A disconnect ends the link at once, so a listener is ready
// for the next connection before the peer can dial again.
func (l *gattLink) watch() error {
	l.mu.Lock()
	paths := []dbus.ObjectPath{l.device}
	if !l.peripheral {
		paths = append(paths, l.characteristic)
	}
	l.mu.Unlock()
	for _, path := range paths {
		if err := l.r.conn.AddMatchSignal(dbus.WithMatchSender(bluez), dbus.WithMatchObjectPath(path),
			dbus.WithMatchInterface(propertiesIF), dbus.WithMatchMember("PropertiesChanged")); err != nil {
			return err
		}
	}
	// One indication is in flight at a time, so this buffer does not fill.
	// D-Bus drops a signal that does not fit; a lost packet then fails
	// decryption and closes the link, never passes silently.
	signals := make(chan *dbus.Signal, 512)
	l.r.conn.Signal(signals)
	go l.dispatch(signals, paths[0])
	return nil
}

func (l *gattLink) dispatch(signals chan *dbus.Signal, device dbus.ObjectPath) {
	defer l.r.conn.RemoveSignal(signals)
	for {
		select {
		case <-l.ctx.Done():
			return
		case s, ok := <-signals:
			if !ok {
				l.cancel()
				return
			}
			if s.Name != propertiesIF+".PropertiesChanged" || len(s.Body) < 2 {
				continue
			}
			iface, _ := s.Body[0].(string)
			changed, _ := s.Body[1].(map[string]dbus.Variant)
			switch {
			case s.Path == device && iface == deviceIF:
				if v, ok := changed["Connected"]; ok {
					if up, _ := v.Value().(bool); !up {
						l.cancel()
						return
					}
				}
			case s.Path != device && iface == charIF && !l.peripheral:
				v, ok := changed["Value"]
				if !ok {
					continue
				}
				b, _ := v.Value().([]byte)
				if len(b) == 0 || len(b) > maxPacket {
					l.cancel()
					return
				}
				select {
				case l.in <- b:
				default:
					// A full queue ends the link; it never drops a packet.
					l.cancel()
					return
				}
			}
		}
	}
}

// subscribe asks the peripheral for indications. It runs before the first
// write, so the peripheral has a subscriber before it answers.
func (l *gattLink) subscribe(ctx context.Context) error {
	if err := l.r.Call(ctx, l.characteristic, charIF+".StartNotify"); err != nil {
		return err
	}
	l.notifying = true
	return nil
}

func (l *gattLink) write(ctx context.Context, b []byte) error {
	if len(b) == 0 || len(b) > l.payload() {
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
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.ctx.Done():
		return nil, ErrDisconnected
	case b := <-l.in:
		return b, nil
	}
}

// pump sends queued packets to the central as indications, one at a time. It
// waits for each confirmation, so a slow central slows the link instead of
// losing packets. A missing confirmation ends the link.
func (l *gattLink) pump(value *prop.Properties) {
	select {
	case <-l.ctx.Done():
		return
	case <-l.subscribed:
	}
	for {
		select {
		case <-l.ctx.Done():
			return
		case b := <-l.out:
			value.SetMust(charIF, "Value", b)
			timer := time.NewTimer(confirmTimeout)
			select {
			case <-l.ctx.Done():
				timer.Stop()
				return
			case <-l.confirm:
				timer.Stop()
			case <-timer.C:
				l.cancel()
				return
			}
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
			l.mu.Lock()
			l.device = path
			l.mu.Unlock()
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
					// BlueZ 5.62 and later report the negotiated MTU here.
					l.mu.Lock()
					l.size = packetSize(int(property[uint16](p, "MTU")))
					l.mu.Unlock()
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
		directData:    {charIF: {"UUID": dbus.MakeVariant(directDataUUID), "Service": dbus.MakeVariant(directService), "Flags": dbus.MakeVariant([]string{"write", "indicate"})}},
	}
	if err := l.r.conn.Export(&objectManager{managed}, directApp, managerIF); err != nil {
		return err
	}
	if err := l.r.conn.Export(&directCharacteristic{l}, directData, charIF); err != nil {
		return err
	}
	var value *prop.Properties
	for path, ifs := range managed {
		props := prop.Map{}
		for iface, values := range ifs {
			props[iface] = map[string]*prop.Prop{}
			for name, v := range values {
				props[iface][name] = &prop.Prop{Value: v.Value(), Writable: false, Emit: prop.EmitFalse}
			}
		}
		if path == directData {
			// BlueZ sends each change of Value to the subscribed central.
			props[charIF]["Value"] = &prop.Prop{Value: []byte{}, Writable: false, Emit: prop.EmitTrue}
		}
		p, err := prop.Export(l.r.conn, path, props)
		if err != nil {
			return err
		}
		if path == directData {
			value = p
		}
	}
	go l.pump(value)
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
	if len(value) == 0 || len(value) > maxPacket {
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
	// BlueZ passes the negotiated MTU with each write to a server.
	if mtu, ok := opts["mtu"].Value().(uint16); ok {
		c.l.size = packetSize(int(mtu))
	}
	select {
	case c.l.in <- append([]byte(nil), value...):
		return nil
	default:
		c.l.cancel()
		return fail("InProgress")
	}
}

// StartNotify is BlueZ telling this peripheral that the central enabled
// indications. BlueZ names no device here, so admission stays with WriteValue.
func (c *directCharacteristic) StartNotify(sender dbus.Sender) *dbus.Error {
	if string(sender) != c.l.owner {
		return fail("NotAuthorized")
	}
	c.l.subscribeOnce.Do(func() { close(c.l.subscribed) })
	return nil
}

// StopNotify means the central left; the link ends.
func (c *directCharacteristic) StopNotify(sender dbus.Sender) *dbus.Error {
	if string(sender) != c.l.owner {
		return fail("NotAuthorized")
	}
	c.l.cancel()
	return nil
}

// Confirm is BlueZ reporting that the central confirmed one indication.
func (c *directCharacteristic) Confirm(sender dbus.Sender) *dbus.Error {
	if string(sender) != c.l.owner {
		return fail("NotAuthorized")
	}
	select {
	case c.l.confirm <- struct{}{}:
	default:
	}
	return nil
}

func directSupported() error { return nil }
