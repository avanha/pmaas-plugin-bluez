package scanner

import (
	"fmt"

	dbus "github.com/godbus/dbus/v5"
)

// manufacturerDataOf is BlueZ's manufacturer data, whose values are D-Bus variants, as bytes. A value that is not bytes is
// left out.
func manufacturerDataOf(data map[uint16]interface{}) map[uint16][]byte {
	out := make(map[uint16][]byte, len(data))

	for id, value := range data {
		if bytes, ok := bytesOf(value); ok {
			out[id] = bytes
		}
	}

	return out
}

// serviceDataOf is BlueZ's service data, by UUID, as bytes. A value that is not bytes is left out.
func serviceDataOf(data map[string]interface{}) map[string][]byte {
	out := make(map[string][]byte, len(data))

	for uuid, value := range data {
		if bytes, ok := bytesOf(value); ok {
			out[uuid] = bytes
		}
	}

	return out
}

func bytesOf(value interface{}) ([]byte, bool) {
	if variant, ok := value.(dbus.Variant); ok {
		if variantBytes, ok := variant.Value().([]byte); ok {
			return variantBytes, true
		}

		fmt.Printf("Unknown dbus.Variant type: %T\n", variant.Value())

		return nil, false
	}

	data, ok := value.([]byte)

	return data, ok
}
