package ble

import (
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	appPath     dbus.ObjectPath = "/org/arc/probe"
	servicePath dbus.ObjectPath = appPath + "/service0"
	echoPath    dbus.ObjectPath = servicePath + "/echo"
	advertPath  dbus.ObjectPath = "/org/arc/advertisement"
	advertIF                    = bluez + ".LEAdvertisement1"
)

type sample struct {
	data []byte
	at   time.Time
}

// Echo is intentionally unauthenticated and stores only short-lived test bytes.
// BlueZ invokes exported methods concurrently, so protect the per-device map.
type echoServer struct {
	mu     sync.Mutex
	values map[dbus.ObjectPath]sample
	now    func() time.Time
	owner  string // unique D-Bus name of BlueZ; reject direct calls from other apps
}

func fail(name string) *dbus.Error { return dbus.NewError(bluez+".Error."+name, []any{name}) }

func (s *echoServer) device(sender dbus.Sender, opts map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	if string(sender) != s.owner {
		return "", fail("NotAuthorized")
	}
	device := property[dbus.ObjectPath](opts, "device")
	if !device.IsValid() || device == "/" {
		return "", fail("InvalidArguments")
	}
	if offset, ok := opts["offset"]; ok {
		n, valid := offset.Value().(uint16)
		if !valid || n != 0 {
			return "", fail("InvalidOffset")
		}
	}
	if property[bool](opts, "prepare-authorize") {
		return "", fail("NotSupported")
	}
	return device, nil
}

func (s *echoServer) expire(now time.Time) {
	for device, v := range s.values {
		if !now.Before(v.at.Add(30 * time.Second)) {
			delete(s.values, device)
		}
	}
}

func (s *echoServer) WriteValue(sender dbus.Sender, value []byte, opts map[string]dbus.Variant) *dbus.Error {
	device, err := s.device(sender, opts)
	if err != nil {
		return err
	}
	if len(value) != ProbeBytes {
		return fail("InvalidValueLength")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.expire(now)
	if _, ok := s.values[device]; !ok && len(s.values) >= 16 {
		return fail("InProgress")
	}
	s.values[device] = sample{append([]byte(nil), value...), now}
	return nil
}

func (s *echoServer) ReadValue(sender dbus.Sender, opts map[string]dbus.Variant) ([]byte, *dbus.Error) {
	device, err := s.device(sender, opts)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(s.now())
	v, ok := s.values[device]
	if !ok {
		return nil, fail("Failed")
	}
	return append([]byte(nil), v.data...), nil
}

type objectManager struct{ objects objects }

func (m *objectManager) GetManagedObjects() (objects, *dbus.Error) { return m.objects, nil }

type advertisement struct{}

func (*advertisement) Release() *dbus.Error { return nil }

func serviceObjects() objects {
	return objects{
		servicePath: {serviceIF: {"UUID": dbus.MakeVariant(ServiceUUID), "Primary": dbus.MakeVariant(true)}},
		echoPath:    {charIF: {"UUID": dbus.MakeVariant(EchoUUID), "Service": dbus.MakeVariant(servicePath), "Flags": dbus.MakeVariant([]string{"read", "write"})}},
	}
}
