package bluez

import (
	"sync"
	"time"

	"github.com/avanha/pmaas-spi/bluetooth"
	"github.com/avanha/pmaas-spi/environment"
)

// source is the entity the plugin registers: where its advertisements are heard. It is read from other goroutines and written
// by the scanning one, so it has a lock of its own.
type source struct {
	mu   sync.Mutex
	data bluetooth.Source
}

var _ bluetooth.ISource = (*source)(nil)

func newSource(name string) *source {
	return &source{data: bluetooth.Source{Name: name, Kind: "bluez"}}
}

func (s *source) GetSourceData() bluetooth.Source {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.data
}

// scanning notes that a discovery session is running, or failed to start.
func (s *source) scanning(running bool, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if running {
		if s.data.Connectivity != environment.ConnectivityOnline {
			s.data.Connectivity, s.data.OnlineSince = environment.ConnectivityOnline, at
		}

		s.data.Scanning = bluetooth.ScanActive

		return
	}

	if s.data.Connectivity != environment.ConnectivityOffline {
		s.data.Connectivity, s.data.OfflineSince = environment.ConnectivityOffline, at
	}

	s.data.Scanning = bluetooth.ScanOff
}

// sessionEnded notes that a session ended normally, which it does every couple of minutes. The adapter is still there.
func (s *source) sessionEnded() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data.Scanning = bluetooth.ScanOff
}

func (s *source) heard(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data.Heard++
	s.data.LastHeard = at
}

func (s *source) forwarded() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data.Forwarded++
}

func (s *source) dropped() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data.Dropped++
}
