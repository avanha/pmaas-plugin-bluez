// Package bluez is a source of Bluetooth advertisements: it scans with the local radio through BlueZ, over D-Bus, and says
// what it hears (see pmaas-spi/bluetooth). It does not know what any device is: the bluetooth plugin takes the advertisements
// and works that out, and tells this plugin which ones it wants so that the rest are dropped here.
//
// Linux only in practice, since BlueZ is. On another system the scan fails to start, the source reports itself offline, and
// nothing else happens, so one assembly can serve both.
package bluez

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/avanha/pmaas-plugin-bluez/internal/scanner"
	"github.com/avanha/pmaas-plugin-bluez/internal/translate"
	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/bluetooth"
	"github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/events"
	"github.com/muka/go-bluetooth/api"
)

type Plugin interface {
	spi.IPMAASPlugin
}

// startScanFunc starts a discovery session on the adapter: the channel its observations arrive on, which is closed when it
// has ended, and a function that stops it and waits for that.
type startScanFunc func(adapterId string) (chan translate.Observation, func(), error)

type plugin struct {
	config    PluginConfig
	container spi.IPMAASContainer
	coalescer *bluetooth.Coalescer
	source    *bluetooth.SourceState

	// startScan is how a session is started: BlueZ's, except in tests.
	startScan startScanFunc

	// defaultAdapter is the adapter to use when the configuration asks for the default, found when the plugin starts.
	defaultAdapter func() string

	sourceEntityId  string
	receiverHandle  int
	receiverStarted bool

	// stopRun stops the scanning goroutine and waits for it. Owned by the plugin goroutine.
	stopRun func()
}

// Force implementation of spi.IPMAASPlugin
var _ spi.IPMAASPlugin = (*plugin)(nil)

func NewPlugin(config PluginConfig) Plugin {
	config = config.withDefaults()

	return &plugin{
		config:    config,
		coalescer: bluetooth.NewCoalescer(),
		source:    bluetooth.NewSourceState(config.Name, "bluez"),
		startScan: func(adapterId string) (chan translate.Observation, func(), error) {
			return scanner.New().StartScan(adapterId)
		},
		defaultAdapter: api.GetDefaultAdapterID,
	}
}

func (p *plugin) ShortName() string {
	return "bluez"
}

func (p *plugin) Init(container spi.IPMAASContainer) {
	p.container = container
}

func (p *plugin) Start() {
	fmt.Printf("%T Starting...\n", p)

	id, err := p.container.RegisterEntity(p.config.Name, bluetooth.ISourceType, p.config.Name,
		func() (any, error) { return p.source, nil })
	if err != nil {
		fmt.Printf("%T unable to register the source: %v\n", p, err)
	} else {
		p.sourceEntityId = id
	}

	handle, err := p.container.RegisterEventReceiver(bluetooth.InterestPredicate, p.onInterest)
	if err != nil {
		// Nothing would be forwarded, since nobody could say what they want.
		fmt.Printf("%T unable to listen for what is wanted, so nothing will be forwarded: %v\n", p, err)
	} else {
		p.receiverHandle, p.receiverStarted = handle, true
	}

	adapterId := p.config.Adapter
	if adapterId == DefaultAdapter {
		adapterId = p.defaultAdapter()
	}

	// The scanning goroutine is given this, so it never reads what Stop clears.
	sourceId := p.sourceEntityId

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		p.run(ctx, adapterId, sourceId)
	}()

	p.stopRun = func() {
		cancel()
		<-runDone
	}

	fmt.Printf("%T Started, scanning with %s\n", p, adapterId)
}

// onInterest is told what consumers want, on the plugin goroutine.
func (p *plugin) onInterest(eventInfo *events.EventInfo) error {
	event, ok := eventInfo.Event.(bluetooth.InterestEvent)
	if !ok {
		return nil
	}

	if err := p.coalescer.SetInterest(event); err != nil {
		fmt.Printf("%T ignoring an interest from %q: %v\n", p, event.Owner, err)
	}

	return nil
}

// Stop runs on the plugin goroutine, which it must not block: stopping the scan and waiting for it takes as long as the
// session's own teardown. So the wait happens on a background goroutine, and the channel Stop returns is closed once it is
// done, which is how the core is told the plugin has completely finished.
func (p *plugin) Stop() chan func() {
	fmt.Printf("%T Stopping...\n", p)

	if p.receiverStarted {
		p.receiverStarted = false

		if err := p.container.DeregisterEventReceiver(p.receiverHandle); err != nil {
			fmt.Printf("%T unable to stop listening for what is wanted: %v\n", p, err)
		}
	}

	stopRun := p.stopRun
	p.stopRun = nil

	sourceEntityId := p.sourceEntityId
	p.sourceEntityId = ""

	// Never started, or already stopped: there is nothing to wait for.
	if stopRun == nil {
		p.deregisterSource(sourceEntityId)

		return p.container.ClosedCallbackChannel()
	}

	done := make(chan func())

	go func() {
		defer close(done)

		// The scanning goroutine may still be broadcasting, so the source is only deregistered once it has stopped, and
		// before the channel is closed: closing it is how the core knows this goroutine will touch nothing more.
		stopRun()
		p.deregisterSource(sourceEntityId)
		fmt.Printf("%T Stopped\n", p)
	}()

	return done
}

func (p *plugin) deregisterSource(id string) {
	if id == "" {
		return
	}

	if err := p.container.DeregisterEntity(id); err != nil {
		fmt.Printf("%T unable to deregister the source: %v\n", p, err)
	}
}

// run scans until it is told to stop: a discovery session, a pause, and again. Each session starts BlueZ's view of the world
// afresh, which is what flushes what it has accumulated.
func (p *plugin) run(ctx context.Context, adapterId string, sourceId string) {
	for {
		sessionCtx, cancel := context.WithTimeout(ctx, p.config.sessionLength)
		err := p.session(sessionCtx, adapterId, sourceId)
		cancel()

		if err != nil {
			fmt.Printf("%T the scan failed, trying again in %v: %s\n", p, p.config.pause, err)
		}

		if !wait(ctx, p.config.pause) {
			return
		}
	}
}

// session runs one discovery session until the context is done, handing what it hears on.
func (p *plugin) session(ctx context.Context, adapterId string, sourceId string) error {
	observationCh, stopScan, err := p.startScan(adapterId)
	if err != nil {
		p.sourceDown(time.Now())

		return fmt.Errorf("unable to start scanning: %w", err)
	}

	p.sourceUp(time.Now())

	consumed := make(chan struct{})

	go func() {
		defer close(consumed)

		translator := translate.New(sourceId, p.coalescer)

		for o := range observationCh {
			p.handle(translator, o)
		}
	}()

	// Until the session is over, or BlueZ ends it: a session that has stopped delivering is not scanning, and waiting
	// out its time would leave nothing listening.
	endedEarly := false

	select {
	case <-ctx.Done():
	case <-consumed:
		endedEarly = true
	}

	stopScan()
	<-consumed
	p.source.SetScanning(bluetooth.ScanOff)

	if endedEarly {
		return errors.New("BlueZ ended the discovery session")
	}

	return nil
}

// sourceUp notes that a discovery session is running.
func (p *plugin) sourceUp(at time.Time) {
	p.source.SetConnectivity(environment.ConnectivityOnline, at)
	p.source.SetScanning(bluetooth.ScanActive)
}

// sourceDown notes that a discovery session could not be started.
func (p *plugin) sourceDown(at time.Time) {
	p.source.SetConnectivity(environment.ConnectivityOffline, at)
	p.source.SetScanning(bluetooth.ScanOff)
}

// handle takes in one observation and forwards what is wanted of it.
func (p *plugin) handle(translator *translate.Translator, o translate.Observation) {
	p.source.Heard(o.Time)

	result := translator.Translate(o)

	if result.Sighting != nil {
		if err := bluetooth.PublishSighting(p.container, *result.Sighting); err != nil {
			p.source.Dropped()
			fmt.Printf("%T unable to broadcast a sighting of %s: %v\n", p, result.Sighting.Address, err)

			return
		}

		p.source.Sighted()

		return
	}

	if result.Verdict != bluetooth.Forward {
		return
	}

	if err := bluetooth.PublishAdvertisement(p.container, result.Event); err != nil {
		p.source.Dropped()
		fmt.Printf("%T unable to broadcast an advertisement from %s: %v\n", p, result.Event.Address, err)

		return
	}

	p.source.Forwarded()
}

// wait waits for the time, or until the context is done, and says whether the time passed.
func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
