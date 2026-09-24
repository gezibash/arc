package ble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func options(device string) map[string]dbus.Variant {
	return map[string]dbus.Variant{"device": dbus.MakeVariant(dbus.ObjectPath(device))}
}

func TestEchoIsolationAndLimits(t *testing.T) {
	now := time.Now()
	s := &echoServer{values: map[dbus.ObjectPath]sample{}, now: func() time.Time { return now }, owner: ":1.7"}
	payload := bytes.Repeat([]byte{42}, ProbeBytes)
	if err := s.WriteValue(":1.8", payload, options("/dev1")); err == nil {
		t.Fatal("accepted other D-Bus caller")
	}
	if err := s.WriteValue(":1.7", payload[:19], options("/dev1")); err == nil {
		t.Fatal("accepted wrong length")
	}
	opts := options("/dev1")
	opts["offset"] = dbus.MakeVariant(uint16(1))
	if err := s.WriteValue(":1.7", payload, opts); err == nil {
		t.Fatal("accepted offset")
	}
	opts = options("/dev1")
	opts["prepare-authorize"] = dbus.MakeVariant(true)
	if err := s.WriteValue(":1.7", payload, opts); err == nil {
		t.Fatal("accepted prepared write")
	}
	if err := s.WriteValue(":1.7", payload, options("/dev1")); err != nil {
		t.Fatal(err)
	}
	payload[0] = 0
	got, err := s.ReadValue(":1.7", options("/dev1"))
	if err != nil || got[0] != 42 {
		t.Fatal("write aliases buffer")
	}
	got[0] = 0
	got, _ = s.ReadValue(":1.7", options("/dev1"))
	if got[0] != 42 {
		t.Fatal("read aliases buffer")
	}
	if _, err = s.ReadValue(":1.7", options("/dev2")); err == nil {
		t.Fatal("peer read another peer's data")
	}
	for i := 2; i <= 16; i++ {
		if err = s.WriteValue(":1.7", payload, options(fmt.Sprintf("/dev%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.WriteValue(":1.7", payload, options("/dev17")); err == nil {
		t.Fatal("unbounded peer map")
	}
	now = now.Add(30 * time.Second)
	if _, err = s.ReadValue(":1.7", options("/dev1")); err == nil || len(s.values) != 0 {
		t.Fatal("did not expire")
	}
}

func TestConcurrentEcho(t *testing.T) {
	s := &echoServer{values: map[dbus.ObjectPath]sample{}, now: time.Now, owner: ":1.7"}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			opts := options(fmt.Sprintf("/dev%d", i))
			value := bytes.Repeat([]byte{byte(i)}, ProbeBytes)
			for j := 0; j < 100; j++ {
				if err := s.WriteValue(":1.7", value, opts); err != nil {
					t.Error(err)
					return
				}
				got, err := s.ReadValue(":1.7", opts)
				if err != nil || !bytes.Equal(got, value) {
					t.Error("concurrent peer data changed")
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

const testDevice dbus.ObjectPath = "/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF"
const testChar dbus.ObjectPath = testDevice + "/service/char"

func remoteObjects() objects {
	service := dbus.ObjectPath(string(testDevice) + "/service")
	return objects{
		testDevice: {deviceIF: {"Adapter": dbus.MakeVariant(dbus.ObjectPath("/org/bluez/hci0")), "Address": dbus.MakeVariant("AA:BB:CC:DD:EE:FF"), "UUIDs": dbus.MakeVariant([]string{ServiceUUID}), "ServicesResolved": dbus.MakeVariant(true)}},
		service:    {serviceIF: {"UUID": dbus.MakeVariant(ServiceUUID), "Device": dbus.MakeVariant(testDevice)}},
		testChar:   {charIF: {"UUID": dbus.MakeVariant(EchoUUID), "Service": dbus.MakeVariant(service), "Flags": dbus.MakeVariant([]string{"read", "write"})}},
	}
}

type fakeRadio struct {
	all     objects
	calls   []string
	payload []byte
	corrupt bool
	fail    string
}

func (f *fakeRadio) Call(ctx context.Context, _ dbus.ObjectPath, method string, args ...any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.calls = append(f.calls, method)
	if method == f.fail {
		return errors.New("test failure")
	}
	if strings.HasSuffix(method, ".WriteValue") {
		f.payload = append([]byte(nil), args[0].([]byte)...)
		if property[string](args[1].(map[string]dbus.Variant), "type") != "request" {
			return errors.New("missing acknowledged write")
		}
	}
	return nil
}
func (f *fakeRadio) Objects(ctx context.Context) (objects, error) { return f.all, ctx.Err() }
func (f *fakeRadio) Read(ctx context.Context, _ dbus.ObjectPath) ([]byte, error) {
	out := append([]byte(nil), f.payload...)
	if f.corrupt {
		out[0] ^= 1
	}
	return out, ctx.Err()
}
func TestProbeLifecycle(t *testing.T) {
	for _, failure := range []string{"", deviceIF + ".Connect", charIF + ".WriteValue", "corrupt"} {
		t.Run(failure, func(t *testing.T) {
			r := &fakeRadio{all: remoteObjects(), fail: failure, corrupt: failure == "corrupt"}
			var output bytes.Buffer
			o, _ := normalize(Options{Peer: "AA:BB:CC:DD:EE:FF", Timeout: time.Second, Output: &output})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := discover(ctx, r, o)
			if (err != nil) != (failure != "") {
				t.Fatalf("result %v", err)
			}
			calls := strings.Join(r.calls, " ")
			if !strings.Contains(calls, ".Disconnect") || !strings.HasSuffix(calls, ".StopDiscovery") {
				t.Fatalf("cleanup missing: %s", calls)
			}
			if failure == "" && (!strings.Contains(output.String(), "PASS") || len(r.payload) != ProbeBytes) {
				t.Fatal("round trip not reported")
			}
		})
	}
}
func TestCancellationAndFiltering(t *testing.T) {
	r := &fakeRadio{all: remoteObjects()}
	r.all[testDevice][deviceIF]["ServicesResolved"] = dbus.MakeVariant(false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := exchange(ctx, r, testDevice); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("did not time out: %v", err)
	}
	if !strings.HasSuffix(strings.Join(r.calls, " "), ".Disconnect") {
		t.Fatal("cancel did not disconnect")
	}
	r.all = remoteObjects()
	r.all[testDevice][deviceIF]["Connected"] = dbus.MakeVariant(true)
	r.calls = nil
	if err := exchange(context.Background(), r, testDevice); err == nil || len(r.calls) != 0 {
		t.Fatal("touched existing connection")
	}
	all := remoteObjects()
	all[dbus.ObjectPath(string(testDevice)+"/service")][serviceIF]["Device"] = dbus.MakeVariant(dbus.ObjectPath("/another"))
	if _, err := characteristic(all, testDevice); err == nil {
		t.Fatal("selected another device's service")
	}
	for _, o := range []Options{{Adapter: "../hci0", Timeout: time.Second}, {Peer: "invalid", Timeout: time.Second}, {Timeout: 0}} {
		if _, err := normalize(o); err == nil {
			t.Fatal("invalid option accepted")
		}
	}
}

func TestAdvertisingLifecycle(t *testing.T) {
	for _, failure := range []string{"", bluez + ".GattManager1.RegisterApplication", bluez + ".LEAdvertisingManager1.RegisterAdvertisement"} {
		r := &fakeRadio{fail: failure}
		called := false
		err := advertiseAndRun(context.Background(), r, "/org/bluez/hci0", func() error { called = true; return nil })
		if (err != nil) != (failure != "") || called != (failure == "") {
			t.Fatalf("failure %s: %v", failure, err)
		}
		calls := strings.Join(r.calls, " ")
		if failure != bluez+".GattManager1.RegisterApplication" && !strings.HasSuffix(calls, ".UnregisterApplication") {
			t.Fatal("application registration leaked")
		}
		if failure == "" && !strings.Contains(calls, ".UnregisterAdvertisement") {
			t.Fatal("advertisement leaked")
		}
	}
	if sig := dbus.SignatureOf(serviceObjects()).String(); sig != "a{oa{sa{sv}}}" {
		t.Fatalf("invalid ObjectManager signature %s", sig)
	}
}
