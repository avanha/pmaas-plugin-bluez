package scanner

import (
	"testing"

	dbus "github.com/godbus/dbus/v5"
)

func TestManufacturerDataOf_VariantsAndPlainBytesAreBytesAndOtherThingsAreLeftOut(t *testing.T) {
	got := manufacturerDataOf(map[uint16]interface{}{
		1: dbus.MakeVariant([]byte{1, 2}),
		2: []byte{3},
		3: dbus.MakeVariant("text"),
		4: 5,
	})

	if len(got) != 2 || len(got[1]) != 2 || got[2][0] != 3 {
		t.Errorf("%v", got)
	}

	if len(manufacturerDataOf(nil)) != 0 {
		t.Error("nil became something")
	}
}

func TestServiceDataOf(t *testing.T) {
	got := serviceDataOf(map[string]interface{}{"a": dbus.MakeVariant([]byte{1}), "b": "no"})

	if len(got) != 1 || got["a"][0] != 1 {
		t.Errorf("%v", got)
	}
}
