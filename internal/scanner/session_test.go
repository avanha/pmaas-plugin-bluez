package scanner

import (
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/avanha/pmaas-plugin-bluez/internal/translate"
	dbus "github.com/godbus/dbus/v5"
)

const (
	hci0   = dbus.ObjectPath("/org/bluez/hci0")
	devA   = dbus.ObjectPath("/org/bluez/hci0/dev_A4_C1_38_00_00_01")
	devB   = dbus.ObjectPath("/org/bluez/hci0/dev_A4_C1_38_00_00_02")
	other  = dbus.ObjectPath("/org/bluez/hci1/dev_A4_C1_38_00_00_03")
	t0     = "2026-10-18T08:00:00Z"
	addrA  = "A4:C1:38:00:00:01"
	addrB  = "A4:C1:38:00:00:02"
	uuidFF = "0000ec88-0000-1000-8000-00805f9b34fb"
)

// fakeBus is BlueZ as a session sees it: calls it records, and signals the test sends.
type fakeBus struct {
	mu         sync.Mutex
	signals    chan *dbus.Signal
	started    []dbus.ObjectPath
	stopped    []dbus.ObjectPath
	fetched    []dbus.ObjectPath
	closed     int
	ended      bool
	startErr   error
	properties map[dbus.ObjectPath]map[string]dbus.Variant
}

func newFakeBus() *fakeBus {
	return &fakeBus{signals: make(chan *dbus.Signal, 100), properties: map[dbus.ObjectPath]map[string]dbus.Variant{}}
}

func (b *fakeBus) StartDiscovery(a dbus.ObjectPath) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.started = append(b.started, a)

	return b.startErr
}

func (b *fakeBus) StopDiscovery(a dbus.ObjectPath) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.stopped = append(b.stopped, a)

	return nil
}

func (b *fakeBus) Signals() <-chan *dbus.Signal { return b.signals }

func (b *fakeBus) DeviceProperties(d dbus.ObjectPath) (map[string]dbus.Variant, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.fetched = append(b.fetched, d)

	p, ok := b.properties[d]
	if !ok {
		return nil, errors.New("no such device")
	}

	return p, nil
}

func (b *fakeBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.closed++
	b.endLocked()

	return nil
}

// endLocked ends the signals, as the connection ending does, once.
func (b *fakeBus) endLocked() {
	if !b.ended {
		b.ended = true
		close(b.signals)
	}
}

func (b *fakeBus) end() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.endLocked()
}

func (b *fakeBus) set(path dbus.ObjectPath, props map[string]dbus.Variant) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.properties[path] = props
}

func (b *fakeBus) fetchedPaths() []dbus.ObjectPath {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]dbus.ObjectPath(nil), b.fetched...)
}

func variants(m map[string]any) map[string]dbus.Variant {
	out := map[string]dbus.Variant{}
	for k, v := range m {
		out[k] = dbus.MakeVariant(v)
	}

	return out
}

func deviceProps(address string) map[string]dbus.Variant {
	return variants(map[string]any{"Address": address, "AddressType": "public"})
}

func interfacesAdded(path dbus.ObjectPath, props map[string]dbus.Variant) *dbus.Signal {
	return &dbus.Signal{Name: signalInterfacesAdded, Path: "/", Body: []any{path, map[string]map[string]dbus.Variant{deviceIface: props}}}
}

func interfacesRemoved(path dbus.ObjectPath) *dbus.Signal {
	return &dbus.Signal{Name: signalInterfacesRemoved, Path: "/", Body: []any{path, []string{deviceIface}}}
}

func changed(path dbus.ObjectPath, props map[string]dbus.Variant) *dbus.Signal {
	return &dbus.Signal{Name: signalPropertiesChanged, Path: path, Body: []any{deviceIface, props, []string{}}}
}

func start(t *testing.T) (*fakeBus, chan translate.Observation, func()) {
	t.Helper()

	b := newFakeBus()
	s := New(slog.New(slog.DiscardHandler))
	s.connect = func(adapter dbus.ObjectPath) (bus, error) {
		if adapter != hci0 {
			t.Errorf("connected for %v", adapter)
		}

		return b, nil
	}
	s.now = func() time.Time { return time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC) }

	ch, cancel, err := s.StartScan("hci0")
	if err != nil {
		t.Fatal(err)
	}

	return b, ch, cancel
}

func next(t *testing.T, ch chan translate.Observation) translate.Observation {
	t.Helper()

	select {
	case o, ok := <-ch:
		if !ok {
			t.Fatal("the channel closed")
		}

		return o
	case <-time.After(2 * time.Second):
		t.Fatal("no observation")
	}

	return translate.Observation{}
}

func closedWithin(t *testing.T, ch chan translate.Observation) {
	t.Helper()

	select {
	case o, ok := <-ch:
		if ok {
			t.Fatalf("an observation: %+v", o)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the channel did not close")
	}
}

func TestStartScan_StartsDiscoveryOnTheAdapterAndStopsAndClosesWhenCancelled(t *testing.T) {
	b, ch, cancel := start(t)

	if len(b.started) != 1 || b.started[0] != hci0 {
		t.Fatalf("started %v", b.started)
	}

	cancel()
	closedWithin(t, ch)
	cancel() // twice is harmless

	if len(b.stopped) != 1 || b.closed != 1 {
		t.Errorf("stopped %v, closed %d", b.stopped, b.closed)
	}
}

func TestStartScan_AFailureToConnectOrToStartIsAnErrorAndLeavesNothingOpen(t *testing.T) {
	s := New(slog.New(slog.DiscardHandler))
	s.connect = func(dbus.ObjectPath) (bus, error) { return nil, errors.New("no bus") }

	if _, _, err := s.StartScan("hci0"); err == nil {
		t.Error("connected to nothing")
	}

	b := newFakeBus()
	b.startErr = errors.New("not ready")
	s.connect = func(dbus.ObjectPath) (bus, error) { return b, nil }

	if _, _, err := s.StartScan("hci0"); err == nil || b.closed != 1 {
		t.Errorf("err %v, closed %d", err, b.closed)
	}
}

func TestSession_ADeviceAddedIsObservedWhole(t *testing.T) {
	b, ch, cancel := start(t)
	defer cancel()

	b.signals <- interfacesAdded(devA, variants(map[string]any{
		"Address": addrA, "AddressType": "random", "Name": "GVH5075_0001", "RSSI": int16(-61), "UUIDs": []string{uuidFF},
		"ManufacturerData": map[uint16]dbus.Variant{0xec88: dbus.MakeVariant([]byte{1, 2, 3})},
		"ServiceData":      map[string]dbus.Variant{uuidFF: dbus.MakeVariant([]byte{9})},
	}))

	o := next(t, ch)

	if o.Kind != translate.Added || o.Address != addrA || o.AddressType != "random" || o.Name != "GVH5075_0001" || o.RSSI != -61 ||
		len(o.UUIDs) != 1 || o.UUIDs[0] != uuidFF || len(o.ManufacturerData[0xec88]) != 3 || o.ServiceData[uuidFF][0] != 9 || o.Time.IsZero() {
		t.Errorf("%+v", o)
	}

	// What changes about it afterwards needs nothing asked of BlueZ.
	b.signals <- changed(devA, variants(map[string]any{"RSSI": int16(-70)}))

	if o := next(t, ch); o.Kind != translate.RSSI || o.RSSI != -70 || o.Address != addrA || o.AddressType != "random" {
		t.Errorf("%+v", o)
	}

	if len(b.fetchedPaths()) != 0 {
		t.Errorf("asked BlueZ about %v", b.fetchedPaths())
	}
}

func TestSession_ADeviceBlueZAlreadyHadIsAskedAboutOnceAndItsChangesAreObservedInOrder(t *testing.T) {
	b, ch, cancel := start(t)
	defer cancel()

	b.set(devA, deviceProps(addrA))

	b.signals <- changed(devA, variants(map[string]any{
		"RSSI": int16(-50), "Name": "n", "UUIDs": []string{uuidFF},
		"ManufacturerData": map[uint16]dbus.Variant{1: dbus.MakeVariant([]byte{7})},
		"ServiceData":      map[string]dbus.Variant{uuidFF: dbus.MakeVariant([]byte{8})},
		"Unrelated":        true,
	}))

	var kinds []translate.Kind

	for i := 0; i < 5; i++ {
		o := next(t, ch)
		kinds = append(kinds, o.Kind)

		if o.Address != addrA || o.AddressType != "public" {
			t.Errorf("%+v", o)
		}
	}

	want := []translate.Kind{translate.Name, translate.UUIDs, translate.ServiceData, translate.ManufacturerData, translate.RSSI}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds %v, want %v", kinds, want)
		}
	}

	b.signals <- changed(devA, variants(map[string]any{"RSSI": int16(-51)}))
	next(t, ch)

	if got := b.fetchedPaths(); len(got) != 1 || got[0] != devA {
		t.Errorf("asked BlueZ about %v", got)
	}
}

func TestSession_ADeviceBlueZCantDescribeIsLeftOutAndTheSessionGoesOn(t *testing.T) {
	b, ch, cancel := start(t)
	defer cancel()

	b.signals <- changed(devA, variants(map[string]any{"RSSI": int16(-50)})) // no such device
	b.set(devB, deviceProps(addrB))
	b.signals <- changed(devB, variants(map[string]any{"RSSI": int16(-40)}))

	if o := next(t, ch); o.Address != addrB {
		t.Errorf("%+v", o)
	}
}

func TestSession_ADeviceRemovedIsObservedAndForgotten(t *testing.T) {
	b, ch, cancel := start(t)
	defer cancel()

	b.signals <- interfacesAdded(devA, deviceProps(addrA))
	next(t, ch)

	b.signals <- interfacesRemoved(devA)

	if o := next(t, ch); o.Kind != translate.Removed || o.Address != addrA {
		t.Errorf("%+v", o)
	}

	// Removing what isn't known says nothing, and a device that comes back is asked about afresh.
	b.signals <- interfacesRemoved(devB)
	b.set(devA, deviceProps(addrA))
	b.signals <- changed(devA, variants(map[string]any{"RSSI": int16(-1)}))

	if o := next(t, ch); o.Kind != translate.RSSI {
		t.Errorf("%+v", o)
	}
}

func TestSession_WhatIsNotUnderTheAdapterOrNotADeviceIsIgnored(t *testing.T) {
	b, ch, cancel := start(t)
	defer cancel()

	b.set(other, deviceProps("A4:C1:38:00:00:03"))
	b.signals <- interfacesAdded(other, deviceProps("A4:C1:38:00:00:03"))
	b.signals <- changed(other, variants(map[string]any{"RSSI": int16(-1)}))
	b.signals <- &dbus.Signal{Name: signalPropertiesChanged, Path: devA, Body: []any{"org.bluez.Other1", variants(map[string]any{"RSSI": int16(-1)}), []string{}}}
	b.signals <- &dbus.Signal{Name: signalInterfacesAdded, Path: "/", Body: []any{devA, map[string]map[string]dbus.Variant{"org.bluez.GattService1": {}}}}
	b.signals <- &dbus.Signal{Name: signalInterfacesAdded, Path: "/", Body: []any{"malformed"}}
	b.signals <- &dbus.Signal{Name: "some.other.Signal"}
	b.signals <- nil
	b.signals <- interfacesAdded(devB, deviceProps(addrB)) // the first thing that counts

	if o := next(t, ch); o.Address != addrB {
		t.Errorf("%+v", o)
	}

	if len(b.fetchedPaths()) != 0 {
		t.Errorf("asked BlueZ about %v", b.fetchedPaths())
	}
}

func TestSession_BlueZEndingTheDiscoveryOrLeavingTheBusEndsTheSession(t *testing.T) {
	for name, signal := range map[string]*dbus.Signal{
		"discovery stopped": {Name: signalPropertiesChanged, Path: hci0, Body: []any{adapterIface, variants(map[string]any{"Discovering": false}), []string{}}},
		"bluetoothd gone":   {Name: signalNameOwnerChanged, Path: "/org/freedesktop/DBus", Body: []any{bluezName, ":1.5", ""}},
	} {
		b, ch, cancel := start(t)
		b.signals <- signal
		closedWithin(t, ch)
		cancel()

		_ = name
	}

	// Other changes to the adapter, and other names, are not the end.
	b, ch, cancel := start(t)
	defer cancel()

	b.signals <- &dbus.Signal{Name: signalPropertiesChanged, Path: hci0, Body: []any{adapterIface, variants(map[string]any{"Discovering": true}), []string{}}}
	b.signals <- &dbus.Signal{Name: signalPropertiesChanged, Path: hci0, Body: []any{adapterIface, variants(map[string]any{"Powered": false}), []string{}}}
	// Another adapter stopping is not this one stopping.
	b.signals <- &dbus.Signal{Name: signalPropertiesChanged, Path: "/org/bluez/hci1", Body: []any{adapterIface, variants(map[string]any{"Discovering": false}), []string{}}}
	b.signals <- &dbus.Signal{Name: signalNameOwnerChanged, Body: []any{"org.other", ":1.5", ""}}
	b.signals <- &dbus.Signal{Name: signalNameOwnerChanged, Body: []any{bluezName, "", ":1.6"}}
	b.signals <- interfacesAdded(devA, deviceProps(addrA))

	if o := next(t, ch); o.Address != addrA {
		t.Errorf("%+v", o)
	}
}

func TestSession_TheConnectionEndingEndsTheSession(t *testing.T) {
	b, ch, cancel := start(t)
	defer cancel()

	b.end()
	closedWithin(t, ch)
}

func TestSession_CancellingWhileNobodyIsReadingDoesNotHang(t *testing.T) {
	b, ch, cancel := start(t)

	// More than the channel holds, with nobody reading it.
	for i := 0; i < 30; i++ {
		b.signals <- interfacesAdded(dbus.ObjectPath("/org/bluez/hci0/dev_X"+string(rune('A'+i%20))), deviceProps(addrA))
	}

	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})

	go func() {
		cancel()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel hung on a session waiting to hand on an observation")
	}

	for range ch { // drained, and closed
	}
}

func TestFirstAdapter(t *testing.T) {
	adapter := map[string]map[string]dbus.Variant{adapterIface: {}}
	device := map[string]map[string]dbus.Variant{deviceIface: {}}

	if got := firstAdapter(map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		"/org/bluez/hci1": adapter, "/org/bluez/hci0": adapter, "/org/bluez/hci0/dev_X": device,
	}); got != "hci0" {
		t.Errorf("got %q", got)
	}

	if got := firstAdapter(map[dbus.ObjectPath]map[string]map[string]dbus.Variant{"/org/bluez/hci0/dev_X": device}); got != fallbackAdapter {
		t.Errorf("with no adapter, %q", got)
	}
}

func TestPropertyReaders_TakeWhatIsThereAndSayNothingOtherwise(t *testing.T) {
	if stringOf(dbus.MakeVariant("a")) != "a" || stringOf(dbus.MakeVariant(1)) != "" || stringOf(dbus.Variant{}) != "" {
		t.Error("stringOf")
	}

	if rssiOf(dbus.MakeVariant(int16(-5))) != -5 || rssiOf(dbus.MakeVariant("x")) != 0 {
		t.Error("rssiOf")
	}

	if len(uuidsOf(dbus.MakeVariant([]string{"a"}))) != 1 || uuidsOf(dbus.MakeVariant(1)) != nil {
		t.Error("uuidsOf")
	}

	if len(manufacturerVariants(dbus.MakeVariant(map[uint16]dbus.Variant{1: dbus.MakeVariant([]byte{1})}))) != 1 || manufacturerVariants(dbus.MakeVariant(1)) != nil {
		t.Error("manufacturerVariants")
	}

	if len(serviceVariants(dbus.MakeVariant(map[string]dbus.Variant{"a": dbus.MakeVariant([]byte{1})}))) != 1 || serviceVariants(dbus.MakeVariant(1)) != nil {
		t.Error("serviceVariants")
	}
}
