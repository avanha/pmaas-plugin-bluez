// Package scanner is the part that is BlueZ: it runs a discovery session on an adapter, watches the devices BlueZ finds, and
// reports what it sees as observations (see the translate package). It decides nothing.
//
// It talks to BlueZ over D-Bus, directly, with godbus: a discovery session is a few calls on the adapter and three kinds of
// signal, so a Bluetooth library on top of that would be more to keep up with than the thing it saves.
package scanner

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/avanha/pmaas-plugin-bluez/internal/translate"
	dbus "github.com/godbus/dbus/v5"
)

const (
	bluezName       = "org.bluez"
	adapterPrefix   = "/org/bluez/"
	adapterIface    = "org.bluez.Adapter1"
	deviceIface     = "org.bluez.Device1"
	propertiesIface = "org.freedesktop.DBus.Properties"

	signalPropertiesChanged = propertiesIface + ".PropertiesChanged"
	signalInterfacesAdded   = "org.freedesktop.DBus.ObjectManager.InterfacesAdded"
	signalInterfacesRemoved = "org.freedesktop.DBus.ObjectManager.InterfacesRemoved"
	signalNameOwnerChanged  = "org.freedesktop.DBus.NameOwnerChanged"
)

// fallbackAdapter is the adapter used when BlueZ can't be asked which there are: the first one, as the kernel names them.
const fallbackAdapter = "hci0"

// bus is what a session needs of the connection to BlueZ, which is D-Bus, and which a test can stand in for.
type bus interface {
	// StartDiscovery has the adapter scan for low energy devices, powering it on if it is off.
	StartDiscovery(adapter dbus.ObjectPath) error

	// StopDiscovery has it stop.
	StopDiscovery(adapter dbus.ObjectPath) error

	// Signals are the signals a session reads, already asked for: changes to the properties of what is under the adapter, objects
	// appearing and disappearing, and bluetoothd leaving the bus. The channel is closed when the connection ends.
	Signals() <-chan *dbus.Signal

	// DeviceProperties is everything BlueZ holds about a device.
	DeviceProperties(device dbus.ObjectPath) (map[string]dbus.Variant, error)

	Close() error
}

// connectFunc makes the connection to BlueZ for a session on the adapter.
type connectFunc func(adapter dbus.ObjectPath) (bus, error)

// Scanner is one discovery session. It is used once: make another for the next.
type Scanner struct {
	log     *slog.Logger
	connect connectFunc
	now     func() time.Time
}

func New(log *slog.Logger) *Scanner {
	valueLog = log

	return &Scanner{log: log, connect: connectSystemBus, now: time.Now}
}

// StartScan starts discovery on the adapter, which is named as the kernel does, "hci0". Observations arrive on the channel, which is
// closed when the session has ended, whether it was stopped or BlueZ ended it; the function stops the session and waits for that.
func (s *Scanner) StartScan(adapterId string) (chan translate.Observation, func(), error) {
	adapter := dbus.ObjectPath(adapterPrefix + adapterId)

	b, err := s.connect(adapter)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to connect to BlueZ for adapter \"%s\": %w", adapterId, err)
	}

	if err := b.StartDiscovery(adapter); err != nil {
		_ = b.Close()

		return nil, nil, fmt.Errorf("unable to start discovery on adapter \"%s\": %w", adapterId, err)
	}

	observationCh := make(chan translate.Observation, 10)
	stop := make(chan struct{})
	done := make(chan struct{})

	sess := &session{log: s.log, bus: b, adapter: adapter, now: s.now, out: observationCh, stop: stop, devices: make(map[dbus.ObjectPath]*known)}

	go func() {
		defer close(done)
		defer close(observationCh)

		sess.run()
	}()

	var once sync.Once

	cancel := func() {
		once.Do(func() {
			close(stop)

			if err := b.StopDiscovery(adapter); err != nil {
				s.log.Warn("unable to stop discovery", "error", err)
			}

			// Closing the connection ends the signals, which is what a session waiting on them is woken by.
			if err := b.Close(); err != nil {
				s.log.Warn("error closing the connection to BlueZ", "error", err)
			}
		})

		<-done
		s.log.Info("discovery stopped")
	}

	return observationCh, cancel, nil
}

// known is a device the session has met: what doesn't change about it, which each observation of it carries.
type known struct {
	address     string
	addressType string
}

// session reads the signals of one discovery session and turns them into observations.
type session struct {
	log     *slog.Logger
	bus     bus
	adapter dbus.ObjectPath
	now     func() time.Time
	out     chan translate.Observation
	stop    chan struct{}
	devices map[dbus.ObjectPath]*known
}

// run reads signals until the session is stopped, the connection ends, or BlueZ ends the discovery.
func (s *session) run() {
	s.log.Debug("reading discovery events")
	defer s.log.Debug("finished reading discovery events")

	signals := s.bus.Signals()

	for {
		select {
		case <-s.stop:
			return
		case signal, ok := <-signals:
			if !ok {
				return
			}

			if signal == nil {
				continue
			}

			if ended := s.handle(signal); ended {
				return
			}
		}
	}
}

// emit hands an observation on, unless the session is being stopped: nobody is reading then.
func (s *session) emit(o translate.Observation) {
	select {
	case s.out <- o:
	case <-s.stop:
	}
}

func (s *session) underAdapter(path dbus.ObjectPath) bool {
	return strings.HasPrefix(string(path), string(s.adapter)+"/")
}

// handle deals with one signal, and reports whether it ended the session.
func (s *session) handle(signal *dbus.Signal) bool {
	switch signal.Name {
	case signalInterfacesAdded:
		s.interfacesAdded(signal)
	case signalInterfacesRemoved:
		s.interfacesRemoved(signal)
	case signalPropertiesChanged:
		return s.propertiesChanged(signal)
	case signalNameOwnerChanged:
		return s.nameOwnerChanged(signal)
	}

	return false
}

func (s *session) interfacesAdded(signal *dbus.Signal) {
	if len(signal.Body) < 2 {
		return
	}

	path, ok1 := signal.Body[0].(dbus.ObjectPath)
	interfaces, ok2 := signal.Body[1].(map[string]map[string]dbus.Variant)

	if !ok1 || !ok2 || !s.underAdapter(path) {
		return
	}

	properties, ok := interfaces[deviceIface]
	if !ok {
		return
	}

	s.log.Debug("received a device added", "path", path)

	device := &known{address: stringOf(properties["Address"]), addressType: stringOf(properties["AddressType"])}
	s.devices[path] = device

	s.emit(translate.Observation{
		Kind:             translate.Added,
		Time:             s.now(),
		Address:          device.address,
		AddressType:      device.addressType,
		Name:             stringOf(properties["Name"]),
		RSSI:             rssiOf(properties["RSSI"]),
		UUIDs:            uuidsOf(properties["UUIDs"]),
		ManufacturerData: manufacturerDataOf(manufacturerVariants(properties["ManufacturerData"])),
		ServiceData:      serviceDataOf(serviceVariants(properties["ServiceData"])),
	})
}

func (s *session) interfacesRemoved(signal *dbus.Signal) {
	if len(signal.Body) < 2 {
		return
	}

	path, ok1 := signal.Body[0].(dbus.ObjectPath)
	removed, ok2 := signal.Body[1].([]string)

	if !ok1 || !ok2 || !s.underAdapter(path) {
		return
	}

	for _, iface := range removed {
		if iface != deviceIface {
			continue
		}

		s.log.Debug("received a device removed", "path", path)

		device, ok := s.devices[path]
		if !ok {
			s.log.Debug("device record not found", "path", path)

			return
		}

		delete(s.devices, path)
		s.emit(translate.Observation{Kind: translate.Removed, Time: s.now(), Address: device.address})

		return
	}
}

// propertiesChanged reports whether the change ended the session: the adapter's discovery has stopped.
func (s *session) propertiesChanged(signal *dbus.Signal) bool {
	if len(signal.Body) < 2 {
		return false
	}

	iface, ok1 := signal.Body[0].(string)
	changed, ok2 := signal.Body[1].(map[string]dbus.Variant)

	if !ok1 || !ok2 {
		return false
	}

	switch {
	case iface == adapterIface && signal.Path == s.adapter:
		if discovering, ok := changed["Discovering"]; ok {
			if on, isBool := discovering.Value().(bool); isBool && !on {
				s.log.Warn("BlueZ stopped discovering", "adapter", s.adapter)

				return true
			}
		}
	case iface == deviceIface && s.underAdapter(signal.Path):
		s.deviceChanged(signal.Path, changed)
	}

	return false
}

func (s *session) deviceChanged(path dbus.ObjectPath, changed map[string]dbus.Variant) {
	device, ok := s.devices[path]
	if !ok {
		// A device BlueZ already had when the session began: it is told of by its changes, and what doesn't change is asked for.
		properties, err := s.bus.DeviceProperties(path)
		if err != nil {
			s.log.Error("failed to get device properties", "path", path, "error", err)

			return
		}

		device = &known{address: stringOf(properties["Address"]), addressType: stringOf(properties["AddressType"])}
		s.devices[path] = device
	}

	base := translate.Observation{Time: s.now(), Address: device.address, AddressType: device.addressType}

	// The name and the services before the reading, and the signal last: a reading that arrives with them is then
	// attributed to a device that is known.
	if v, ok := changed["Name"]; ok {
		o := base
		o.Kind, o.Name = translate.Name, stringOf(v)
		s.emit(o)
	}

	if v, ok := changed["UUIDs"]; ok {
		o := base
		o.Kind, o.UUIDs = translate.UUIDs, uuidsOf(v)
		s.emit(o)
	}

	if v, ok := changed["ServiceData"]; ok {
		o := base
		o.Kind, o.ServiceData = translate.ServiceData, serviceDataOf(serviceVariants(v))
		s.emit(o)
	}

	if v, ok := changed["ManufacturerData"]; ok {
		o := base
		o.Kind, o.ManufacturerData = translate.ManufacturerData, manufacturerDataOf(manufacturerVariants(v))
		s.emit(o)
	}

	if v, ok := changed["RSSI"]; ok {
		o := base
		o.Kind, o.RSSI = translate.RSSI, rssiOf(v)
		s.emit(o)
	}
}

// nameOwnerChanged reports whether bluetoothd left the bus, which is the end of every session.
func (s *session) nameOwnerChanged(signal *dbus.Signal) bool {
	if len(signal.Body) < 3 {
		return false
	}

	name, ok1 := signal.Body[0].(string)
	newOwner, ok2 := signal.Body[2].(string)

	if ok1 && ok2 && name == bluezName && newOwner == "" {
		s.log.Warn("BlueZ left the bus")

		return true
	}

	return false
}
