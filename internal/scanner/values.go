package scanner

import (
	"fmt"
	"log/slog"

	dbus "github.com/godbus/dbus/v5"
)

// valueLog is where the functions that read values out of BlueZ's properties log, which belong to no scanner. It is set by New.
var valueLog = slog.New(slog.DiscardHandler)

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

		valueLog.Debug("unknown dbus.Variant type", "type", fmt.Sprintf("%T", variant.Value()))

		return nil, false
	}

	data, ok := value.([]byte)

	return data, ok
}

// stringOf is a property that is a string, or empty if it is not one or is not there.
func stringOf(v dbus.Variant) string {
	s, _ := v.Value().(string)

	return s
}

// rssiOf is the signal strength property, or 0 if it is not there.
func rssiOf(v dbus.Variant) int {
	switch n := v.Value().(type) {
	case int16:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

// uuidsOf is the service UUIDs property.
func uuidsOf(v dbus.Variant) []string {
	uuids, _ := v.Value().([]string)

	return uuids
}

// manufacturerVariants is the manufacturer data property as manufacturerDataOf takes it.
func manufacturerVariants(v dbus.Variant) map[uint16]interface{} {
	data, ok := v.Value().(map[uint16]dbus.Variant)
	if !ok {
		return nil
	}

	out := make(map[uint16]interface{}, len(data))
	for id, value := range data {
		out[id] = value
	}

	return out
}

// serviceVariants is the service data property as serviceDataOf takes it.
func serviceVariants(v dbus.Variant) map[string]interface{} {
	data, ok := v.Value().(map[string]dbus.Variant)
	if !ok {
		return nil
	}

	out := make(map[string]interface{}, len(data))
	for uuid, value := range data {
		out[uuid] = value
	}

	return out
}
