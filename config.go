package bluez

import "time"

// DefaultAdapter asks for the system's default Bluetooth adapter.
const DefaultAdapter string = "DEFAULT_ADAPTER"

// PluginConfig configures the plugin.
type PluginConfig struct {
	// Adapter is the BlueZ adapter to scan with, such as "hci0". The default is the system's default adapter.
	Adapter string

	// Name is what the source is called on the Bluetooth page. The default is "Local Bluetooth".
	Name string

	// sessionLength is how long a discovery session runs before a new one is started, and pause how long to wait between
	// them. Restarting is what flushes BlueZ's cache of what it has heard. Zero means the defaults; tests shorten them.
	sessionLength time.Duration
	pause         time.Duration
}

func NewPluginConfig() PluginConfig {
	return PluginConfig{Adapter: DefaultAdapter, Name: "Local Bluetooth"}
}

const (
	defaultSessionLength = 120 * time.Second
	defaultPause         = 5 * time.Second
)

func (c PluginConfig) withDefaults() PluginConfig {
	if c.Adapter == "" {
		c.Adapter = DefaultAdapter
	}

	if c.Name == "" {
		c.Name = "Local Bluetooth"
	}

	if c.sessionLength <= 0 {
		c.sessionLength = defaultSessionLength
	}

	if c.pause <= 0 {
		c.pause = defaultPause
	}

	return c
}
