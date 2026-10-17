// Package scanner is the part that is BlueZ: it runs a discovery session on an adapter, watches the properties of the devices
// BlueZ finds, and reports what it sees as observations (see the translate package). It decides nothing.
package scanner

import (
	"fmt"
	"time"

	"github.com/avanha/pmaas-plugin-bluez/internal/translate"
	dbus "github.com/godbus/dbus/v5"
	"github.com/muka/go-bluetooth/api"
	"github.com/muka/go-bluetooth/bluez"
	"github.com/muka/go-bluetooth/bluez/profile/adapter"
	"github.com/muka/go-bluetooth/bluez/profile/device"
)

// Scanner is one discovery session. It is used once: make another for the next.
type Scanner struct {
	devices            map[dbus.ObjectPath]*watched
	watchConsumerCount int
}

// watched is a device BlueZ has found, and the channel its property changes come on.
type watched struct {
	path             dbus.ObjectPath
	address          string
	device           *device.Device1
	propertyChangeCh chan *bluez.PropertyChanged
}

type propertyChange struct {
	bluezEvent *bluez.PropertyChanged
	device     *watched
	eventTime  time.Time
	newValue   interface{}
}

func New() *Scanner {
	return &Scanner{devices: make(map[dbus.ObjectPath]*watched)}
}

// StartScan starts discovery on the adapter. Observations arrive on the channel, which is closed when the session has ended; the
// function stops the session and waits for that.
func (s *Scanner) StartScan(adapterId string) (chan translate.Observation, func(), error) {
	adapter1, err := api.GetAdapter(adapterId)
	if err != nil {
		return nil, nil, fmt.Errorf("error starting scan, unable to get adapter \"%s\": %w", adapterId, err)
	}

	fmt.Printf("Retrieved adapter interface %v\n", adapter1.Path())

	var filter *adapter.DiscoveryFilter

	discoveryCh, discoverCancelFunc, err := api.Discover(adapter1, filter)
	if err != nil {
		return nil, nil, fmt.Errorf("error starting scan, unable to start discovery: %w", err)
	}

	observationCh := make(chan translate.Observation, 10)
	scanDone := make(chan error)

	go s.readDiscoveryEvents(discoveryCh, observationCh, scanDone)

	cancelFunc := func() {
		discoverCancelFunc()

		if err := <-scanDone; err == nil {
			fmt.Println("Discovery stopped")
		} else {
			fmt.Printf("Discovery stopped with error: %s", err)
		}

		// Don't call api.Exit(): no more calls can be made to BlueZ afterwards.
	}

	return observationCh, cancelFunc, nil
}

func (s *Scanner) readDiscoveryEvents(
	discoveryCh chan *adapter.DeviceDiscovered,
	observationCh chan translate.Observation,
	scanDoneCh chan error) {
	fmt.Println("Start readDiscoveryEvents")

	propChangeCh := make(chan *propertyChange, 100)
	propChangeConsumerDoneCh := make(chan *watched)

LOOP:
	for {
		select {
		case discoveryEv := <-discoveryCh:
			if discoveryEv == nil {
				fmt.Println("Received nil discovery event")
				break LOOP
			}

			s.handleDiscoveryEvent(discoveryEv, time.Now(), propChangeCh, observationCh, propChangeConsumerDoneCh)
		case propChangeEv := <-propChangeCh:
			s.handlePropChangeEvent(propChangeEv, observationCh)
		case dev := <-propChangeConsumerDoneCh:
			s.watchConsumerCount--
			fmt.Printf("PropChange consumer for %s is complete, %d active consumers\n", dev.path, s.watchConsumerCount)
		}
	}

	// At this point, there will be no more discovery events: unwatch any currently present devices.
	for path, dev := range s.devices {
		dev.device.UnwatchProperties(dev.propertyChangeCh)
		delete(s.devices, path)
	}

	if s.watchConsumerCount == 0 {
		// There are no active property watch consumers, so close the channels now.
		close(propChangeCh)
		close(propChangeConsumerDoneCh)
	} else {
		// Keep consuming property change events until all the consumers have stopped.
	REMLOOP:
		for {
			select {
			case propChangeEv := <-propChangeCh:
				s.handlePropChangeEvent(propChangeEv, observationCh)
			case dev := <-propChangeConsumerDoneCh:
				s.watchConsumerCount--
				fmt.Printf("PropChange consumer for %s is complete, %d active consumers\n", dev.path, s.watchConsumerCount)

				if s.watchConsumerCount == 0 {
					break REMLOOP
				}
			}
		}

		close(propChangeCh)
		close(propChangeConsumerDoneCh)

		// Consume what was left in the property change channel's buffer.
		for propChangeEv := range propChangeCh {
			s.handlePropChangeEvent(propChangeEv, observationCh)
		}
	}

	close(observationCh)
	close(scanDoneCh)
	fmt.Println("End readDiscoveryEvents")
}

func (s *Scanner) handleDiscoveryEvent(
	ev *adapter.DeviceDiscovered,
	eventTime time.Time,
	propChangeCh chan *propertyChange,
	observationCh chan translate.Observation,
	propChangeConsumerDoneCh chan *watched) {
	switch ev.Type {
	case adapter.DeviceRemoved:
		fmt.Printf("Received DeviceRemoved event, path: %s\n", ev.Path)

		dev, ok := s.devices[ev.Path]
		if !ok {
			fmt.Printf("Device record not found, path: %s\n", ev.Path)

			return
		}

		dev.device.UnwatchProperties(dev.propertyChangeCh)
		delete(s.devices, ev.Path)
		observationCh <- translate.Observation{Kind: translate.Removed, Time: eventTime, Address: dev.address}
	case adapter.DeviceAdded:
		existing, ok := s.devices[ev.Path]
		if ok {
			existing.device.UnwatchProperties(existing.propertyChangeCh)
			delete(s.devices, ev.Path)
		}

		d, err := device.NewDevice1(ev.Path)
		if err != nil {
			fmt.Printf("Failed to get device interface for path %s: %s\n", ev.Path, err)

			return
		}

		dev := &watched{path: ev.Path, address: d.Properties.Address, device: d}
		s.devices[ev.Path] = dev
		s.watchDevice(dev, propChangeCh, propChangeConsumerDoneCh)

		observationCh <- translate.Observation{
			Kind:             translate.Added,
			Time:             eventTime,
			Address:          d.Properties.Address,
			AddressType:      d.Properties.AddressType,
			Name:             d.Properties.Name,
			RSSI:             int(d.Properties.RSSI),
			UUIDs:            d.Properties.UUIDs,
			ManufacturerData: manufacturerDataOf(d.Properties.ManufacturerData),
			ServiceData:      serviceDataOf(d.Properties.ServiceData),
		}
	default:
		fmt.Printf("Received %s event, path: %s\n", deviceActionName(ev.Type), ev.Path)
	}
}

// deviceActionName names a device discovery action for logging. go-bluetooth's DeviceActions is a bare
// uint8 with no String method, so formatting it directly prints something like "%!s(adapter.DeviceActions=0)".
func deviceActionName(action adapter.DeviceActions) string {
	switch action {
	case adapter.DeviceAdded:
		return "device added"
	case adapter.DeviceRemoved:
		return "device removed"
	default:
		return fmt.Sprintf("unknown device action (%d)", uint8(action))
	}
}

func (s *Scanner) watchDevice(dev *watched, propChangeCh chan *propertyChange, propChangeConsumerDoneCh chan *watched) {
	ch, err := dev.device.WatchProperties()
	if err != nil {
		fmt.Printf("Failed to watch properties of %s (%s): %s\n", dev.device.Properties.Name, dev.address, err)

		return
	}

	// Saved so that UnwatchProperties can be called when the device is removed, or when it is time to shut down.
	dev.propertyChangeCh = ch
	s.watchConsumerCount++

	go func() {
		for evt := range ch {
			if evt == nil {
				break
			}

			// Capture the new value now, in this goroutine, before sending it on to the one that consumes the channel.
			var newValue interface{}

			switch evt.Name {
			case "ManufacturerData":
				newValue = dev.device.Properties.ManufacturerData
			case "RSSI":
				newValue = dev.device.Properties.RSSI
			case "UUIDs":
				newValue = dev.device.Properties.UUIDs
			case "ServiceData":
				newValue = dev.device.Properties.ServiceData
			case "Name":
				newValue = dev.device.Properties.Name
			}

			propChangeCh <- &propertyChange{bluezEvent: evt, device: dev, eventTime: time.Now(), newValue: newValue}
		}

		propChangeConsumerDoneCh <- dev
	}()
}

func (s *Scanner) handlePropChangeEvent(evt *propertyChange, observationCh chan translate.Observation) {
	o := translate.Observation{
		Time:        evt.eventTime,
		Address:     evt.device.address,
		AddressType: evt.device.device.Properties.AddressType,
	}

	switch evt.bluezEvent.Name {
	case "ManufacturerData":
		data, _ := evt.newValue.(map[uint16]interface{})
		o.Kind, o.ManufacturerData = translate.ManufacturerData, manufacturerDataOf(data)
	case "ServiceData":
		data, _ := evt.newValue.(map[string]interface{})
		o.Kind, o.ServiceData = translate.ServiceData, serviceDataOf(data)
	case "UUIDs":
		o.Kind, o.UUIDs = translate.UUIDs, evt.newValue.([]string)
	case "Name":
		o.Kind, o.Name = translate.Name, evt.newValue.(string)
	case "RSSI":
		o.Kind, o.RSSI = translate.RSSI, int(evt.newValue.(int16))
	default:
		return
	}

	observationCh <- o
}
