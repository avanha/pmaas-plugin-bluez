package translate

import (
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/avanha/pmaas-spi/bluetooth"
)

var epoch = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

const (
	goveeAddress   = "A4:C1:38:CC:E2:F0"
	inkbirdAddress = "49:22:01:23:0A:1B"
)

func wanting(t *testing.T, rules ...bluetooth.Rule) *bluetooth.Coalescer {
	t.Helper()

	c := bluetooth.NewCoalescer()
	if err := c.SetInterest(bluetooth.InterestEvent{Owner: "bluetooth", Rules: rules}); err != nil {
		t.Fatal(err)
	}

	return c
}

func goveeRule() bluetooth.Rule {
	return bluetooth.Rule{Name: "govee", ServiceUUID: bluetooth.UUID16(0xec88)}
}

func inkbirdRule() bluetooth.Rule {
	return bluetooth.Rule{Name: "inkbird", ServiceUUID: bluetooth.UUID16(0xfff0), NamePrefix: "sps"}
}

func govee(at time.Duration, reading byte) Observation {
	return Observation{
		Kind: Added, Time: epoch.Add(at), Address: goveeAddress, AddressType: "public", Name: "GVH5075_E2F0", RSSI: -60,
		UUIDs:            []string{bluetooth.UUID16(0xec88)},
		ManufacturerData: map[uint16][]byte{0xec88: {0x00, 0x03, 0x39, reading}, 0x004c: {2, 21}},
	}
}

func TestTranslate_ADeviceAddedBecomesAnAdvertisementWithEverythingBlueZKnows(t *testing.T) {
	tr := New("src", wanting(t, goveeRule()))

	r := tr.Translate(govee(0, 1))
	if r.Verdict != bluetooth.Forward || r.Rule == nil || r.Rule.Name != "govee" {
		t.Fatalf("%v %+v", r.Verdict, r.Rule)
	}

	e := r.Event
	if e.Source != "src" || e.Address != goveeAddress || e.AddressType != bluetooth.AddressPublic || e.RSSI != -60 ||
		!e.Time.Equal(epoch) || e.LocalName != "GVH5075_E2F0" || len(e.ServiceUUIDs) != 1 ||
		len(e.ManufacturerData) != 2 || e.ManufacturerData[0xec88][3] != 1 {
		t.Errorf("event %+v", e)
	}
}

func TestTranslate_NothingIsForwardedUntilSomebodyHasSaidWhatTheyWant(t *testing.T) {
	tr := New("src", bluetooth.NewCoalescer())

	if r := tr.Translate(govee(0, 1)); r.Verdict != bluetooth.NotWanted || r.Rule != nil {
		t.Errorf("%v %+v", r.Verdict, r.Rule)
	}
}

func TestTranslate_ADeviceNobodyWantsIsNotForwarded(t *testing.T) {
	tr := New("src", wanting(t, inkbirdRule()))

	if r := tr.Translate(govee(0, 1)); r.Verdict != bluetooth.NotWanted {
		t.Errorf("%v", r.Verdict)
	}
}

func TestTranslate_OnlyTheRecordsThatAreNewAreSentSoAnAccumulatingMapIsNotMistakenForNews(t *testing.T) {
	tr := New("src", wanting(t, inkbirdRule()))

	added := Observation{
		Kind: Added, Time: epoch, Address: inkbirdAddress, AddressType: "public", Name: "sps",
		UUIDs: []string{bluetooth.UUID16(0xfff0)},
		ManufacturerData: map[uint16][]byte{
			0x0760: {1, 1, 1, 1, 1, 1, 1}, 0x0761: {2, 2, 2, 2, 2, 2, 2}, 0x0762: {3, 3, 3, 3, 3, 3, 3},
		},
	}

	// Three old readings on first sight: all new to us, so all sent, and the parser, seeing three, will wait.
	if r := tr.Translate(added); r.Verdict != bluetooth.Forward || len(r.Event.ManufacturerData) != 3 {
		t.Fatalf("%v %+v", r.Verdict, r.Event.ManufacturerData)
	}

	// BlueZ's map now has a fourth key; the event has only that one.
	changed := added
	changed.Kind, changed.Time = ManufacturerData, epoch.Add(time.Second)
	changed.ManufacturerData = map[uint16][]byte{
		0x0760: {1, 1, 1, 1, 1, 1, 1}, 0x0761: {2, 2, 2, 2, 2, 2, 2}, 0x0762: {3, 3, 3, 3, 3, 3, 3}, 0x0766: {9, 9, 9, 9, 9, 9, 9},
	}

	r := tr.Translate(changed)
	if r.Verdict != bluetooth.Forward || len(r.Event.ManufacturerData) != 1 || r.Event.ManufacturerData[0x0766][0] != 9 {
		t.Fatalf("%v %+v", r.Verdict, r.Event.ManufacturerData)
	}

	// The same map again says nothing new: no event at all.
	if r := tr.Translate(changed); r.Verdict != bluetooth.NotWanted || r.Rule != nil {
		t.Errorf("a map with nothing new: %v %+v", r.Verdict, r.Rule)
	}
}

func TestTranslate_ARecordWhoseDataChangedUnderTheSameKeyIsNew(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := New("src", wanting(t, goveeRule()))

		if r := tr.Translate(govee(0, 1)); r.Verdict != bluetooth.Forward {
			t.Fatal(r.Verdict)
		}

		changed := govee(time.Second, 2)
		changed.Kind = ManufacturerData

		r := tr.Translate(changed)
		if r.Verdict != bluetooth.Forward || len(r.Event.ManufacturerData) != 1 || r.Event.ManufacturerData[0xec88][3] != 2 {
			t.Errorf("%v %+v", r.Verdict, r.Event.ManufacturerData)
		}
	})
}

func TestTranslate_RulesAreMatchedAgainstTheWholeDeviceNotJustWhatChanged(t *testing.T) {
	// The rule needs the name and the service, which came when the device was added, not with the data that changed.
	tr := New("src", wanting(t, inkbirdRule()))

	added := Observation{Kind: Added, Time: epoch, Address: inkbirdAddress, AddressType: "public", Name: "sps", UUIDs: []string{bluetooth.UUID16(0xfff0)}}
	tr.Translate(added)

	data := added
	data.Kind, data.Time = ManufacturerData, epoch.Add(time.Second)
	data.ManufacturerData = map[uint16][]byte{0x0766: {1, 2, 3, 4, 5, 6, 7}}

	r := tr.Translate(data)
	if r.Verdict != bluetooth.Forward || r.Rule == nil || r.Rule.Name != "inkbird" {
		t.Fatalf("%v %+v", r.Verdict, r.Rule)
	}

	// And the event carries the name and service too, so a consumer needs nothing else to identify it.
	if r.Event.LocalName != "sps" || len(r.Event.ServiceUUIDs) != 1 {
		t.Errorf("event %+v", r.Event.Fields)
	}
}

func TestTranslate_ServiceDataUUIDsAndNameChangesAreForwardedWithTheRest(t *testing.T) {
	tr := New("src", wanting(t, goveeRule()))
	tr.Translate(govee(0, 1))

	cases := []struct {
		name  string
		o     Observation
		check func(*testing.T, bluetooth.AdvertisementEvent)
	}{
		{"service data", Observation{Kind: ServiceData, ServiceData: map[string][]byte{bluetooth.UUID16(0xfcd2): {7}}},
			func(t *testing.T, e bluetooth.AdvertisementEvent) {
				if e.ServiceData[bluetooth.UUID16(0xfcd2)][0] != 7 {
					t.Errorf("%+v", e.ServiceData)
				}
			}},
		{"uuids", Observation{Kind: UUIDs, UUIDs: []string{bluetooth.UUID16(0xec88), bluetooth.UUID16(0x180f)}},
			func(t *testing.T, e bluetooth.AdvertisementEvent) {
				if len(e.ServiceUUIDs) != 2 {
					t.Errorf("%+v", e.ServiceUUIDs)
				}
			}},
		{"name", Observation{Kind: Name, Name: "Renamed", UUIDs: nil},
			func(t *testing.T, e bluetooth.AdvertisementEvent) {
				if e.LocalName != "Renamed" {
					t.Errorf("%q", e.LocalName)
				}
			}},
	}

	for i, c := range cases {
		c.o.Address, c.o.Time = goveeAddress, epoch.Add(time.Duration(i+1)*time.Second)

		r := tr.Translate(c.o)
		if r.Verdict != bluetooth.Forward {
			t.Errorf("%s: %v", c.name, r.Verdict)

			continue
		}

		c.check(t, r.Event)
	}
}

func TestTranslate_ASignalChangeAloneIsPassedOnAtMostEveryHalfMinuteAndOnlyForAWantedDevice(t *testing.T) {
	tr := New("src", wanting(t, goveeRule()))
	tr.Translate(govee(0, 1))

	signal := func(at time.Duration, rssi int) Result {
		return tr.Translate(Observation{Kind: RSSI, Time: epoch.Add(at), Address: goveeAddress, RSSI: rssi})
	}

	r := signal(time.Second, -55)
	if r.Verdict != bluetooth.Forward || r.Event.RSSI != -55 || len(r.Event.ManufacturerData) != 0 || r.Event.LocalName != "GVH5075_E2F0" {
		t.Fatalf("%v %+v", r.Verdict, r.Event)
	}

	if r := signal(10*time.Second, -70); r.Verdict == bluetooth.Forward {
		t.Error("passed on again after 9 seconds")
	}

	if r := signal(time.Second+RSSIEvery, -70); r.Verdict != bluetooth.Forward || r.Event.RSSI != -70 {
		t.Errorf("not passed on after the time: %v %+v", r.Verdict, r.Event)
	}

	// A device nobody wants is not passed on, whatever its signal does.
	stranger := Observation{Kind: RSSI, Time: epoch, Address: "11:22:33:44:55:66", RSSI: -50}
	if r := tr.Translate(stranger); r.Verdict == bluetooth.Forward {
		t.Error("a stranger's signal was passed on")
	}
}

func TestTranslate_TheAddressTypeIsMapped(t *testing.T) {
	for word, want := range map[string]bluetooth.AddressType{
		"public": bluetooth.AddressPublic, "random": bluetooth.AddressRandom, "": bluetooth.AddressUnknown, "other": bluetooth.AddressUnknown,
	} {
		tr := New("src", wanting(t, goveeRule()))
		o := govee(0, 1)
		o.AddressType = word

		if got := tr.Translate(o).Event.AddressType; got != want {
			t.Errorf("%q: %v, want %v", word, got, want)
		}
	}
}

func TestTranslate_NothingInTheEventRefersToTheObservation(t *testing.T) {
	tr := New("src", wanting(t, goveeRule()))
	o := govee(0, 1)

	r := tr.Translate(o)

	o.ManufacturerData[0xec88][3] = 99
	o.UUIDs[0] = "changed"

	if r.Event.ManufacturerData[0xec88][3] != 1 || r.Event.ServiceUUIDs[0] != bluetooth.UUID16(0xec88) {
		t.Errorf("the event changed with the observation: %+v", r.Event)
	}
}

func TestTranslate_ARemovedDeviceIsForgottenSoItsRecordsAreNewAgain(t *testing.T) {
	tr := New("src", wanting(t, goveeRule()))
	tr.Translate(govee(0, 1))

	again := govee(time.Second, 1)
	again.Kind = ManufacturerData

	if r := tr.Translate(again); r.Rule != nil {
		t.Fatalf("an old record reported as new: %v", r.Verdict)
	}

	if r := tr.Translate(Observation{Kind: Removed, Address: goveeAddress, Time: epoch}); r.Verdict != bluetooth.NotWanted || r.Rule != nil {
		t.Errorf("a removal produced %v", r.Verdict)
	}

	if _, kept := tr.devices[goveeAddress]; kept {
		t.Error("the device was kept")
	}

	if r := tr.Translate(govee(2*time.Second, 1)); len(r.Event.ManufacturerData) != 2 {
		t.Errorf("%+v", r.Event.ManufacturerData)
	}
}

func TestTranslate_TheNumberOfDevicesKeptIsBounded(t *testing.T) {
	tr := New("src", wanting(t, goveeRule()))

	for i := 0; i < maxDevices+50; i++ {
		o := govee(time.Duration(i)*time.Millisecond, 1)
		o.Address = bluetooth.FormatAddress(uint64(0xA4C138000000 + i))
		tr.Translate(o)
	}

	if len(tr.devices) > maxDevices {
		t.Errorf("%d devices", len(tr.devices))
	}

	// The oldest went.
	if _, kept := tr.devices[bluetooth.FormatAddress(0xA4C138000000)]; kept {
		t.Error("the oldest was kept")
	}
}

func TestTranslate_ARuleOnAKeyBlueZAlreadyHeldMatchesEvenWhenOnlyAnotherKeyIsNew(t *testing.T) {
	// The rule is on Apple's record, which was there from the start; what is new is the other company's reading.
	rule := bluetooth.Rule{Name: "apple", ManufacturerID: bluetooth.ManufacturerOf(0x004c), ManufacturerPrefix: []byte{2, 21}}
	tr := New("src", wanting(t, rule))

	added := govee(0, 1)
	if r := tr.Translate(added); r.Verdict != bluetooth.Forward {
		t.Fatalf("on first sight: %v", r.Verdict)
	}

	changed := govee(time.Second, 2)
	changed.Kind = ManufacturerData

	r := tr.Translate(changed)
	if r.Verdict != bluetooth.Forward || r.Rule == nil || r.Rule.Name != "apple" {
		t.Fatalf("%v %+v", r.Verdict, r.Rule)
	}

	// Only the reading is in the event, though the rule was matched on a record that is not.
	if _, apple := r.Event.ManufacturerData[0x004c]; apple || len(r.Event.ManufacturerData) != 1 {
		t.Errorf("event data %+v", r.Event.ManufacturerData)
	}
}

func TestTranslate_TheDeviceKeepsItsOwnCopyOfWhatBlueZGaveAndWhatItGaveOut(t *testing.T) {
	rule := bluetooth.Rule{Name: "apple", ManufacturerID: bluetooth.ManufacturerOf(0x004c), ManufacturerPrefix: []byte{2, 21}}
	tr := New("src", wanting(t, rule))

	o := govee(0, 1)
	r := tr.Translate(o)

	// Neither BlueZ's map nor the event that was sent is the device's.
	o.ManufacturerData[0x004c][0] = 9
	r.Event.ManufacturerData[0x004c][0] = 9

	signal := tr.Translate(Observation{Kind: RSSI, Time: epoch.Add(time.Minute), Address: goveeAddress, RSSI: -50})
	if signal.Verdict != bluetooth.Forward {
		t.Errorf("the device no longer matches the rule after the copies were changed: %v", signal.Verdict)
	}
}

func sightingsWanted(t *testing.T, rules ...bluetooth.Rule) *bluetooth.Coalescer {
	t.Helper()

	c := bluetooth.NewCoalescer()
	if err := c.SetInterest(bluetooth.InterestEvent{Owner: "bluetooth", Rules: rules, Sightings: time.Minute}); err != nil {
		t.Fatal(err)
	}

	return c
}

func phone(at time.Duration, name string) Observation {
	return Observation{Kind: Added, Time: epoch.Add(at), Address: "44:DC:FA:B6:5B:00", AddressType: "random", Name: name, RSSI: -70}
}

func TestTranslate_ADeviceNobodyWantsIsASightingWhenSightingsAreAskedFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := New("src", sightingsWanted(t, goveeRule()))

		r := tr.Translate(phone(0, ""))
		if r.Verdict != bluetooth.NotWanted || r.Rule != nil || r.Sighting == nil {
			t.Fatalf("%v %+v %+v", r.Verdict, r.Rule, r.Sighting)
		}

		s := r.Sighting
		if s.Source != "src" || s.Address != "44:DC:FA:B6:5B:00" || s.AddressType != bluetooth.AddressRandom || s.RSSI != -70 || !s.Time.Equal(epoch) {
			t.Errorf("sighting %+v", s)
		}

		// The name, when it comes, is told at once.
		named := phone(time.Second, "Phone")
		named.Kind = Name

		if r := tr.Translate(named); r.Sighting == nil || r.Sighting.LocalName != "Phone" {
			t.Errorf("a name learned: %+v", r.Sighting)
		}

		// And nothing again for a minute.
		if r := tr.Translate(phone(2*time.Second, "Phone")); r.Sighting != nil {
			t.Errorf("told again at once: %+v", r.Sighting)
		}
	})
}

func TestTranslate_ASignalChangeOfADeviceNobodyWantsIsASightingAtTheIntervalNotBefore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := New("src", sightingsWanted(t, goveeRule()))
		tr.Translate(phone(0, "Phone"))

		signal := func(rssi int) Result {
			return tr.Translate(Observation{Kind: RSSI, Time: time.Now(), Address: "44:DC:FA:B6:5B:00", RSSI: rssi})
		}

		if r := signal(-80); r.Sighting != nil {
			t.Fatalf("a sighting straight after the first: %+v", r.Sighting)
		}

		time.Sleep(time.Minute)

		// The interest has to be said again, as it is every minute.
		if err := tr.coalescer.SetInterest(bluetooth.InterestEvent{Owner: "bluetooth", Rules: []bluetooth.Rule{goveeRule()}, Sightings: time.Minute}); err != nil {
			t.Fatal(err)
		}

		r := signal(-85)
		if r.Sighting == nil || r.Sighting.RSSI != -85 || r.Verdict == bluetooth.Forward {
			t.Errorf("%v %+v", r.Verdict, r.Sighting)
		}
	})
}

func TestTranslate_AWantedDeviceIsNotASightingAndNoSightingsAreMadeUnlessAskedFor(t *testing.T) {
	tr := New("src", sightingsWanted(t, goveeRule()))

	if r := tr.Translate(govee(0, 1)); r.Verdict != bluetooth.Forward || r.Sighting != nil {
		t.Errorf("a wanted device: %v %+v", r.Verdict, r.Sighting)
	}

	// Advertisements asked for and not sightings.
	quiet := New("src", wanting(t, goveeRule()))

	if r := quiet.Translate(phone(0, "Phone")); r.Sighting != nil {
		t.Errorf("a sighting nobody asked for: %+v", r.Sighting)
	}
}

func TestTranslate_ASightingCarriesWhatTheDeviceHasSaidOverTime(t *testing.T) {
	tr := New("src", sightingsWanted(t, goveeRule()))

	o := phone(0, "Phone")
	o.UUIDs = []string{bluetooth.UUID16(0x180f)}
	o.ManufacturerData = map[uint16][]byte{0x004c: {1, 2}}

	r := tr.Translate(o)
	if r.Sighting == nil || len(r.Sighting.ServiceUUIDs) != 1 || !reflect.DeepEqual(r.Sighting.ManufacturerIDs, []uint16{0x004c}) {
		t.Errorf("%+v", r.Sighting)
	}
}
