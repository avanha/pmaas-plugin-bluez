// Package translate turns what BlueZ says about devices into Bluetooth advertisement events. BlueZ keeps whole devices, with
// properties that it reports changes to; the rest of PMAAS wants advertisements (see pmaas-spi/bluetooth). So this keeps the
// device as BlueZ has it, works out what each change brought, and applies what consumers have said they want.
//
// It knows nothing about D-Bus, so it can be tested without a radio.
package translate

import (
	"fmt"
	"slices"
	"time"

	"github.com/avanha/pmaas-spi/bluetooth"
)

// Kind is what changed.
type Kind int

const (
	// Added is a device BlueZ has just told us about, with everything it knows.
	Added Kind = iota
	ManufacturerData
	ServiceData
	UUIDs
	Name
	RSSI

	// Removed is a device BlueZ has let go of.
	Removed
)

// Observation is something BlueZ told us about a device: the whole device if it was just added, or the new value of the one
// property that changed. The fields not belonging to Kind are ignored, except that Address and Time are always needed.
type Observation struct {
	Kind        Kind
	Time        time.Time
	Address     string
	AddressType string // BlueZ's word: "public" or "random"

	Name  string
	RSSI  int
	UUIDs []string

	// ManufacturerData is the whole of what BlueZ holds, not only what is new: BlueZ keeps every key it has seen during
	// a discovery session, and for some devices (the Inkbird) each reading is a key.
	ManufacturerData map[uint16][]byte
	ServiceData      map[string][]byte
}

// RSSIEvery is the most often a device's signal strength alone is passed on, since BlueZ reports it whenever it changes,
// which is nearly every advertisement.
const RSSIEvery = 30 * time.Second

// maxDevices is how many devices are kept. Past it, the ones not heard from for longest are dropped.
const maxDevices = 2000

type device struct {
	addressType  bluetooth.AddressType
	name         string
	rssi         int
	uuids        []string
	manufacturer map[uint16][]byte
	serviceData  map[string][]byte

	// reported is the manufacturer data records already passed on, which is how what is new in the accumulated map is told.
	reported map[string]bool

	lastHeard   time.Time
	lastRSSIOut time.Time
}

// Translator keeps the devices BlueZ has told it about. Not safe for more than one goroutine.
type Translator struct {
	source    string
	coalescer *bluetooth.Coalescer
	devices   map[string]*device
}

// New makes a Translator whose events say they are from the source (the entity ID of the source), and which applies what the
// Coalescer has been told is wanted.
func New(source string, coalescer *bluetooth.Coalescer) *Translator {
	return &Translator{source: source, coalescer: coalescer, devices: make(map[string]*device)}
}

// Forget drops a device BlueZ has removed.
func (t *Translator) Forget(address string) {
	delete(t.devices, address)
}

// Result is what came of an observation.
type Result struct {
	// Event is the advertisement to broadcast, when Forward.
	Event bluetooth.AdvertisementEvent

	// Verdict says whether it is to be broadcast, was a repeat, or is not wanted. Nothing at all (no change worth
	// reporting) is Verdict NotWanted with a nil Rule.
	Verdict bluetooth.Verdict

	// Rule is the rule that governed it, if any, for counting by.
	Rule *bluetooth.Rule
}

// Translate takes in an observation and says what to do about it.
func (t *Translator) Translate(o Observation) Result {
	if o.Kind == Removed {
		t.Forget(o.Address)

		return Result{}
	}

	d := t.device(o)
	d.lastHeard = o.Time

	var fresh map[uint16][]byte
	var service map[string][]byte

	switch o.Kind {
	case Added:
		d.name, d.rssi = o.Name, o.RSSI
		d.uuids = slices.Clone(o.UUIDs)
		d.manufacturer = cloneRecords(o.ManufacturerData)
		d.serviceData = cloneService(o.ServiceData)
		fresh = d.unreported()
		service = d.serviceData
	case ManufacturerData:
		d.manufacturer = cloneRecords(o.ManufacturerData)

		fresh = d.unreported()
		if len(fresh) == 0 {
			// Nothing that wasn't said before.
			return Result{}
		}
	case ServiceData:
		d.serviceData = cloneService(o.ServiceData)
		service = d.serviceData
	case UUIDs:
		d.uuids = slices.Clone(o.UUIDs)
	case Name:
		d.name = o.Name
	case RSSI:
		d.rssi = o.RSSI

		return t.signalOnly(o, d)
	}

	event := t.eventFor(o, d)
	event.ManufacturerData = fresh
	event.ServiceData = service

	verdict, rule := t.coalescer.AdmitAs(t.viewOf(o, d), &event)

	return Result{Event: event, Verdict: verdict, Rule: rule}
}

// signalOnly is a change in signal strength, which is passed on if the device is wanted at all, and not more than every
// RSSIEvery. It does not go through the Coalescer: it has no data, so it would never match a repeat.
func (t *Translator) signalOnly(o Observation, d *device) Result {
	event := t.eventFor(o, d)

	rule := bluetooth.MatchAny(t.coalescer.Rules(), t.viewOf(o, d))
	if rule == nil || o.Time.Sub(d.lastRSSIOut) < RSSIEvery {
		return Result{}
	}

	d.lastRSSIOut = o.Time

	return Result{Event: event, Verdict: bluetooth.Forward, Rule: rule}
}

// eventFor is the advertisement for the device as it is now, without the data that changed.
func (t *Translator) eventFor(o Observation, d *device) bluetooth.AdvertisementEvent {
	return bluetooth.AdvertisementEvent{
		Source:      t.source,
		Address:     o.Address,
		AddressType: d.addressType,
		RSSI:        d.rssi,
		Time:        o.Time,
		Fields:      bluetooth.Fields{LocalName: d.name, ServiceUUIDs: slices.Clone(d.uuids)},
	}
}

// viewOf is the whole device as one advertisement, for matching the rules against.
func (t *Translator) viewOf(o Observation, d *device) *bluetooth.AdvertisementEvent {
	view := t.eventFor(o, d)
	view.ManufacturerData = d.manufacturer
	view.ServiceData = d.serviceData

	return &view
}

func (t *Translator) device(o Observation) *device {
	d, ok := t.devices[o.Address]
	if ok {
		return d
	}

	if len(t.devices) >= maxDevices {
		t.dropOldest()
	}

	d = &device{addressType: addressTypeOf(o.AddressType), reported: make(map[string]bool)}
	t.devices[o.Address] = d

	return d
}

func (t *Translator) dropOldest() {
	var oldest string
	var oldestHeard time.Time

	for address, d := range t.devices {
		if oldest == "" || d.lastHeard.Before(oldestHeard) {
			oldest, oldestHeard = address, d.lastHeard
		}
	}

	delete(t.devices, oldest)
}

// unreported is the manufacturer data records not passed on before, noting them as passed on. A record is the company ID and
// what follows it, so a reading that changes, or a different key, is new.
func (d *device) unreported() map[uint16][]byte {
	var fresh map[uint16][]byte

	for id, data := range d.manufacturer {
		key := recordKey(id, data)
		if d.reported[key] {
			continue
		}

		d.reported[key] = true

		if fresh == nil {
			fresh = make(map[uint16][]byte)
		}

		fresh[id] = slices.Clone(data)
	}

	return fresh
}

// recordKey identifies a record by its company ID and everything after it.
func recordKey(id uint16, data []byte) string {
	return fmt.Sprintf("%04x:%x", id, data)
}

func addressTypeOf(word string) bluetooth.AddressType {
	switch word {
	case "public":
		return bluetooth.AddressPublic
	case "random":
		return bluetooth.AddressRandom
	default:
		return bluetooth.AddressUnknown
	}
}

func cloneRecords(in map[uint16][]byte) map[uint16][]byte {
	out := make(map[uint16][]byte, len(in))
	for id, data := range in {
		out[id] = slices.Clone(data)
	}

	return out
}

func cloneService(in map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(in))
	for uuid, data := range in {
		out[uuid] = slices.Clone(data)
	}

	return out
}
