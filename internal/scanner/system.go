package scanner

import (
	"fmt"
	"sort"
	"strings"

	dbus "github.com/godbus/dbus/v5"
)

// systemBus is the connection to BlueZ on the system bus: one of its own, so that closing it ends its signals and nothing else.
type systemBus struct {
	conn    *dbus.Conn
	signals chan *dbus.Signal
}

func connectSystemBus(adapter dbus.ObjectPath) (bus, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}

	b := &systemBus{conn: conn, signals: make(chan *dbus.Signal, 256)}

	// Everything under the adapter changing, which is every device's properties in one subscription; devices appearing and
	// going; and bluetoothd leaving the bus.
	matches := [][]dbus.MatchOption{
		{dbus.WithMatchSender(bluezName), dbus.WithMatchInterface(propertiesIface), dbus.WithMatchMember("PropertiesChanged"),
			dbus.WithMatchPathNamespace(adapter)},
		{dbus.WithMatchSender(bluezName), dbus.WithMatchInterface("org.freedesktop.DBus.ObjectManager"),
			dbus.WithMatchMember("InterfacesAdded")},
		{dbus.WithMatchSender(bluezName), dbus.WithMatchInterface("org.freedesktop.DBus.ObjectManager"),
			dbus.WithMatchMember("InterfacesRemoved")},
		{dbus.WithMatchSender("org.freedesktop.DBus"), dbus.WithMatchInterface("org.freedesktop.DBus"),
			dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, bluezName)},
	}

	conn.Signal(b.signals)

	for _, match := range matches {
		if err := conn.AddMatchSignal(match...); err != nil {
			_ = conn.Close()

			return nil, fmt.Errorf("unable to subscribe to BlueZ's signals: %w", err)
		}
	}

	return b, nil
}

func (b *systemBus) Signals() <-chan *dbus.Signal {
	return b.signals
}

func (b *systemBus) Close() error {
	b.conn.RemoveSignal(b.signals)

	err := b.conn.Close()

	// Nothing writes to the channel any more, so it can be closed: a session waiting on it is woken.
	close(b.signals)

	return err
}

func (b *systemBus) object(path dbus.ObjectPath) dbus.BusObject {
	return b.conn.Object(bluezName, path)
}

func (b *systemBus) StartDiscovery(adapter dbus.ObjectPath) error {
	object := b.object(adapter)

	var powered dbus.Variant
	if err := object.Call(propertiesIface+".Get", 0, adapterIface, "Powered").Store(&powered); err != nil {
		return fmt.Errorf("unable to find the adapter: %w", err)
	}

	if on, _ := powered.Value().(bool); !on {
		if err := object.Call(propertiesIface+".Set", 0, adapterIface, "Powered", dbus.MakeVariant(true)).Err; err != nil {
			return fmt.Errorf("unable to power the adapter on: %w", err)
		}
	}

	// Low energy only: the sensors and beacons are, and a scan for classic devices as well keeps the radio busier for nothing. Every
	// advertisement is reported, not only the first of each device, which is what carries a reading that changes.
	filter := map[string]dbus.Variant{
		"Transport":     dbus.MakeVariant("le"),
		"DuplicateData": dbus.MakeVariant(true),
	}

	if err := object.Call(adapterIface+".SetDiscoveryFilter", 0, filter).Err; err != nil {
		return fmt.Errorf("unable to set the discovery filter: %w", err)
	}

	return object.Call(adapterIface+".StartDiscovery", 0).Err
}

func (b *systemBus) StopDiscovery(adapter dbus.ObjectPath) error {
	return b.object(adapter).Call(adapterIface+".StopDiscovery", 0).Err
}

func (b *systemBus) DeviceProperties(device dbus.ObjectPath) (map[string]dbus.Variant, error) {
	var properties map[string]dbus.Variant

	err := b.object(device).Call(propertiesIface+".GetAll", 0, deviceIface).Store(&properties)

	return properties, err
}

// DefaultAdapterID is the adapter to use when none is asked for: the first BlueZ lists, "hci0" if it can't be asked.
func DefaultAdapterID() string {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fallbackAdapter
	}

	defer func() { _ = conn.Close() }()

	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant

	if err := conn.Object(bluezName, "/").Call("org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objects); err != nil {
		return fallbackAdapter
	}

	return firstAdapter(objects)
}

// firstAdapter is the lowest-numbered adapter among BlueZ's objects, or the fallback if there is none.
func firstAdapter(objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant) string {
	var adapters []string

	for path, interfaces := range objects {
		if _, ok := interfaces[adapterIface]; ok && strings.HasPrefix(string(path), adapterPrefix) {
			adapters = append(adapters, strings.TrimPrefix(string(path), adapterPrefix))
		}
	}

	if len(adapters) == 0 {
		return fallbackAdapter
	}

	sort.Strings(adapters)

	return adapters[0]
}
