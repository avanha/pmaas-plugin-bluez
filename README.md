# pmaas-plugin-bluez

A PMAAS plugin that hears Bluetooth LE advertisements with the local radio, through BlueZ over D-Bus, and says what it
hears. It is a *source*: it knows nothing about what any device is. The
[bluetooth plugin](../pmaas-plugin-bluetooth) takes advertisements from this and from any other source (an ESPHome
Bluetooth proxy, in the esphome plugin), works out what the devices are, and publishes them. See that plugin's `DESIGN.md`.

Linux only in practice, since BlueZ is. On another system the scan fails to start, the source reports itself offline, and
nothing else happens, so one assembly can serve both.

```go
conf := bluez.NewPluginConfig()
conf.Adapter = "hci0" // optional; the default is the system's default adapter
coreConfig.AddPlugin(bluez.NewPlugin(conf), config.PluginConfig{})
```

## What it does

* Runs a BlueZ discovery session for two minutes, pauses five seconds, and runs another, which is what flushes what BlueZ has
  accumulated.
* Turns what BlueZ reports (whole devices with properties, and changes to them) into `bluetooth.AdvertisementEvent`s
  (`pmaas-spi/bluetooth`), carrying the device's name and services and only the data that is new. BlueZ keeps every
  manufacturer data key it has seen in a session, and for some devices (the Inkbird) each reading is a key.
* Forwards only what the bluetooth plugin has asked for (`bluetooth.InterestEvent`), applying the rules with the shared
  `Coalescer`, so the many advertisements nobody wants are dropped here. **Until it has been told what is wanted it
  forwards nothing.** A change in signal strength alone is passed on at most every 30 seconds.
* Registers a source entity (`bluetooth.ISource`) with its connectivity and counters (heard, forwarded, dropped).

## Layout

* `internal/scanner`: the BlueZ part. A discovery session, and the properties of the devices it finds, as observations. It talks to
  BlueZ over D-Bus with [godbus](https://github.com/godbus/dbus) directly (no Bluetooth library: the unmaintained ones were more to keep
  up with than the handful of calls and three kinds of signal a session needs). The connection is behind an interface so the
  session logic is tested with signals made up in the test; the real D-Bus part is small.
* `internal/translate`: observations to advertisements. Plain Go, with the tests; no D-Bus.
* `plugin.go`: the sessions, the interest, the source.

## Notes

* The discovery filter asks for low energy devices only, and for every advertisement (`DuplicateData`). BlueZ scans actively with its
  own interval and window, set in `/etc/bluetooth/main.conf`. The adapter is powered on if it is off.
* A session ends when it is stopped, when BlueZ stops discovering (the adapter's `Discovering` goes false), when bluetoothd leaves
  the bus, or when the connection drops; the plugin starts another after its pause.
* BlueZ's D-Bus model reports a property only when it changes, so an advertisement identical to the last is invisible. That
  matters for a button that repeats one frame; see the design's open questions.

## Status page

The plugin's page (`/plugins/bluez/`, "bluez" in the menu) shows the source: whether a discovery session is running and since
when, the adapter, the scan mode, when anything was last heard, how many advertisements it has heard, how many it forwarded to
the Bluetooth plugin, how many devices it told of in sightings, how many it dropped, and how many discovery sessions it has
started. The last failure to start or keep a session, if any, is shown with its time. A radio that is scanning but has heard
nothing for two minutes is flagged: a house is never that quiet, so the radio has probably stopped hearing.
