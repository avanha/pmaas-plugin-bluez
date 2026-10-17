package bluez

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/avanha/pmaas-plugin-bluez/internal/translate"
	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/bluetooth"
	"github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/events"
)

// fakeContainer is what the plugin uses of the container: registrations, the interest receiver, and the broadcasts.
type fakeContainer struct {
	spi.IPMAASContainer

	mu           sync.Mutex
	entities     []string
	deregistered []string
	stubFactory  spi.EntityStubFactoryFunc
	entityType   reflect.Type
	receiver     events.EventReceiver
	predicate    events.EventPredicate
	receiversOff []int
	broadcasts   []bluetooth.AdvertisementEvent
	broadcastErr error
}

func (c *fakeContainer) RegisterEntity(uniqueData string, entityType reflect.Type, _ string, factory spi.EntityStubFactoryFunc) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entities = append(c.entities, uniqueData)
	c.entityType, c.stubFactory = entityType, factory

	return "bluez_1", nil
}

func (c *fakeContainer) DeregisterEntity(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.deregistered = append(c.deregistered, id)

	return nil
}

func (c *fakeContainer) RegisterEventReceiver(predicate events.EventPredicate, receiver events.EventReceiver) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.predicate, c.receiver = predicate, receiver

	return 3, nil
}

func (c *fakeContainer) DeregisterEventReceiver(handle int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.receiver = nil
	c.receiversOff = append(c.receiversOff, handle)

	return nil
}

func (c *fakeContainer) BroadcastEvent(_ string, event any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.broadcastErr != nil {
		return c.broadcastErr
	}

	if a, ok := event.(bluetooth.AdvertisementEvent); ok {
		c.broadcasts = append(c.broadcasts, a)
	}

	return nil
}

func (c *fakeContainer) ClosedCallbackChannel() chan func() {
	ch := make(chan func())
	close(ch)

	return ch
}

func (c *fakeContainer) adverts() []bluetooth.AdvertisementEvent {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]bluetooth.AdvertisementEvent(nil), c.broadcasts...)
}

// sessions scripts the discovery sessions: each call to start is handed the next script.
type sessions struct {
	mu       sync.Mutex
	scripts  []sessionScript
	started  []time.Time
	stopped  int
	adapters []string
}

type sessionScript struct {
	err error

	// feed runs once the session has started. It gives BlueZ's observations to emit, which gives up once the session is
	// stopped, and it returns true if BlueZ ends the session itself. Nil is a session that hears nothing.
	feed func(emit func(translate.Observation))
	end  bool
}

func (s *sessions) start(adapterId string) (chan translate.Observation, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.adapters = append(s.adapters, adapterId)
	s.started = append(s.started, time.Now())

	var script sessionScript
	if len(s.scripts) > 0 {
		script, s.scripts = s.scripts[0], s.scripts[1:]
	}

	if script.err != nil {
		return nil, nil, script.err
	}

	ch := make(chan translate.Observation, 10)
	stopping := make(chan struct{})
	finished := make(chan struct{})

	// As in BlueZ's scanner, the channel is closed by the goroutine that sends on it.
	go func() {
		defer close(finished)
		defer close(ch)

		if script.feed != nil {
			script.feed(func(o translate.Observation) {
				select {
				case ch <- o:
				case <-stopping:
				}
			})
		}

		if !script.end {
			<-stopping
		}
	}()

	var once sync.Once

	return ch, func() {
		once.Do(func() { close(stopping) })
		<-finished

		s.mu.Lock()
		s.stopped++
		s.mu.Unlock()
	}, nil
}

func (s *sessions) startCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.started)
}

func newTestPlugin(t *testing.T, s *sessions) (*plugin, *fakeContainer) {
	t.Helper()

	config := NewPluginConfig()
	config.Adapter = "hci9"

	p := NewPlugin(config).(*plugin)
	p.startScan = s.start
	p.config.sessionLength = 2 * time.Minute
	p.config.pause = 5 * time.Second

	c := &fakeContainer{}
	p.Init(c)

	return p, c
}

func interestIn(t *testing.T, c *fakeContainer, rules ...bluetooth.Rule) {
	t.Helper()

	c.mu.Lock()
	receiver := c.receiver
	c.mu.Unlock()

	if receiver == nil {
		t.Fatal("the plugin isn't listening for what is wanted")
	}

	if err := receiver(&events.EventInfo{Event: bluetooth.InterestEvent{Owner: "bluetooth", Rules: rules}}); err != nil {
		t.Fatal(err)
	}
}

func goveeRule() bluetooth.Rule {
	return bluetooth.Rule{Name: "govee", ServiceUUID: bluetooth.UUID16(0xec88)}
}

func goveeObservation(reading byte) translate.Observation {
	return translate.Observation{
		Kind: translate.Added, Time: time.Now(), Address: "A4:C1:38:CC:E2:F0", AddressType: "public", Name: "GVH5075_E2F0", RSSI: -60,
		UUIDs:            []string{bluetooth.UUID16(0xec88)},
		ManufacturerData: map[uint16][]byte{0xec88: {0, 3, 0x39, reading}},
	}
}

func stop(p *plugin) {
	done := p.Stop()
	if callback, open := <-done; open {
		callback()
	}
}

func TestStart_RegistersTheSourceAndListensForWhatIsWantedAndScansWithTheAdapter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &sessions{}
		p, c := newTestPlugin(t, s)
		p.Start()
		synctest.Wait()

		if !reflect.DeepEqual(c.entities, []string{"Local Bluetooth"}) || c.entityType != bluetooth.ISourceType {
			t.Errorf("entities %v of type %v", c.entities, c.entityType)
		}

		stub, err := c.stubFactory()
		if err != nil {
			t.Fatal(err)
		}

		data := stub.(bluetooth.ISource).GetSourceData()
		if data.Kind != "bluez" || data.Name != "Local Bluetooth" || data.Connectivity != environment.ConnectivityOnline || data.Scanning != bluetooth.ScanActive {
			t.Errorf("source %+v", data)
		}

		if !bluetooth.InterestPredicate(&events.EventInfo{Event: bluetooth.InterestEvent{}}) ||
			!c.predicate(&events.EventInfo{Event: bluetooth.InterestEvent{}}) || c.predicate(&events.EventInfo{Event: "other"}) {
			t.Error("the receiver isn't for interests")
		}

		if len(s.adapters) != 1 || s.adapters[0] != "hci9" {
			t.Errorf("adapters %v", s.adapters)
		}

		stop(p)
	})
}

func TestStart_TheDefaultAdapterIsFoundWhenItStarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &sessions{}
		p, _ := newTestPlugin(t, s)
		p.config.Adapter = DefaultAdapter
		p.defaultAdapter = func() string { return "hci0" }
		p.Start()
		synctest.Wait()

		if len(s.adapters) != 1 || s.adapters[0] != "hci0" {
			t.Errorf("adapters %v", s.adapters)
		}

		stop(p)
	})
}

func TestScanning_NothingIsForwardedUntilSomebodySaysWhatTheyWantAndThenOnlyThat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &sessions{scripts: []sessionScript{{feed: func(emit func(translate.Observation)) {
			emit(goveeObservation(1)) // before anyone has asked

			time.Sleep(10 * time.Second)

			emit(goveeObservation(2))
			emit(translate.Observation{Kind: translate.Added, Time: time.Now(), Address: "11:22:33:44:55:66", AddressType: "random", Name: "A phone"})
		}}}}

		p, c := newTestPlugin(t, s)
		p.Start()
		synctest.Wait()

		if got := c.adverts(); len(got) != 0 {
			t.Fatalf("forwarded before being asked: %+v", got)
		}

		interestIn(t, c, goveeRule())
		time.Sleep(11 * time.Second)
		synctest.Wait()

		got := c.adverts()
		if len(got) != 1 || got[0].Address != "A4:C1:38:CC:E2:F0" || got[0].Source != "bluez_1" || got[0].ManufacturerData[0xec88][3] != 2 {
			t.Fatalf("forwarded %+v", got)
		}

		data := p.source.GetSourceData()
		if data.Heard != 3 || data.Forwarded != 1 || data.Dropped != 0 || data.LastHeard.IsZero() {
			t.Errorf("source %+v", data)
		}

		stop(p)
	})
}

func TestScanning_ABroadcastThatFailsIsCountedAndDoesNotStopTheScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &sessions{scripts: []sessionScript{{feed: func(emit func(translate.Observation)) {
			time.Sleep(time.Second)
			emit(goveeObservation(1))
			time.Sleep(time.Second)
			emit(goveeObservation(2))
		}}}}

		p, c := newTestPlugin(t, s)
		p.Start()
		interestIn(t, c, goveeRule())

		c.mu.Lock()
		c.broadcastErr = errors.New("bus is down")
		c.mu.Unlock()

		time.Sleep(1500 * time.Millisecond)
		synctest.Wait()

		c.mu.Lock()
		c.broadcastErr = nil
		c.mu.Unlock()

		time.Sleep(time.Second)
		synctest.Wait()

		data := p.source.GetSourceData()
		if data.Dropped != 1 || data.Forwarded != 1 || data.Heard != 2 {
			t.Errorf("source %+v", data)
		}

		stop(p)
	})
}

func TestScanning_ASessionEndsAfterItsTimeAndAnotherStartsAfterAPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &sessions{}
		p, _ := newTestPlugin(t, s)
		start := time.Now()
		p.Start()
		synctest.Wait()

		time.Sleep(2*time.Minute + 6*time.Second)
		synctest.Wait()

		s.mu.Lock()
		started, stopped := append([]time.Time(nil), s.started...), s.stopped
		s.mu.Unlock()

		if len(started) != 2 || stopped != 1 {
			t.Fatalf("%d sessions started, %d stopped", len(started), stopped)
		}

		if got := started[1].Sub(start); got != 2*time.Minute+5*time.Second {
			t.Errorf("the second began %v after the first", got)
		}

		stop(p)
	})
}

func TestScanning_ASessionBlueZEndsEarlyIsReplacedAfterThePauseNotAfterItsTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &sessions{scripts: []sessionScript{{end: true, feed: func(func(translate.Observation)) {
			time.Sleep(3 * time.Second) // and then BlueZ ends it
		}}}}

		p, _ := newTestPlugin(t, s)
		start := time.Now()
		p.Start()
		synctest.Wait()

		time.Sleep(10 * time.Second)
		synctest.Wait()

		s.mu.Lock()
		started := append([]time.Time(nil), s.started...)
		s.mu.Unlock()

		if len(started) != 2 || started[1].Sub(start) != 8*time.Second {
			t.Errorf("sessions started at %v after the start", started)
		}

		stop(p)
	})
}

func TestScanning_AnAdapterThatCantBeStartedMakesTheSourceOfflineUntilItCanBe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &sessions{scripts: []sessionScript{{err: errors.New("no adapter")}, {err: errors.New("still no adapter")}}}

		p, _ := newTestPlugin(t, s)
		p.Start()
		synctest.Wait()

		data := p.source.GetSourceData()
		if data.Connectivity != environment.ConnectivityOffline || data.Scanning != bluetooth.ScanOff || data.OfflineSince.IsZero() {
			t.Fatalf("source %+v", data)
		}

		offlineSince := data.OfflineSince

		time.Sleep(11 * time.Second) // the second attempt fails too, and the third works
		synctest.Wait()

		data = p.source.GetSourceData()
		if data.Connectivity != environment.ConnectivityOnline || data.Scanning != bluetooth.ScanActive || data.OnlineSince.IsZero() {
			t.Errorf("source %+v", data)
		}

		if s.startCount() != 3 || !data.OfflineSince.Equal(offlineSince) {
			t.Errorf("%d sessions started, offline since %v then %v", s.startCount(), offlineSince, data.OfflineSince)
		}

		stop(p)
	})
}

func TestScanning_AnInterestNotRefreshedStopsWhatIsForwardedAfterThreeMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &sessions{scripts: []sessionScript{{feed: func(emit func(translate.Observation)) {
			time.Sleep(time.Second)
			emit(goveeObservation(1))

			time.Sleep(time.Minute) // still wanted
			emit(goveeObservation(2))

			time.Sleep(4 * time.Minute) // not any more: the bluetooth plugin would have said so again
			emit(goveeObservation(3))
		}}}}

		p, c := newTestPlugin(t, s)
		p.config.sessionLength = time.Hour
		p.Start()
		interestIn(t, c, goveeRule())

		time.Sleep(10 * time.Minute)
		synctest.Wait()

		got := c.adverts()
		if len(got) != 2 || got[0].ManufacturerData[0xec88][3] != 1 || got[1].ManufacturerData[0xec88][3] != 2 {
			t.Errorf("forwarded %+v", got)
		}

		stop(p)
	})
}

func TestStop_DeregistersEverythingAndOnlyClosesItsChannelAfterTheScanHasStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &sessions{}
		p, c := newTestPlugin(t, s)
		p.Start()
		synctest.Wait()

		done := p.Stop()

		select {
		case <-done:
		case <-time.After(time.Minute):
			t.Fatal("never closed")
		}

		synctest.Wait()

		if !reflect.DeepEqual(c.deregistered, []string{"bluez_1"}) || !reflect.DeepEqual(c.receiversOff, []int{3}) {
			t.Errorf("deregistered %v, receivers %v", c.deregistered, c.receiversOff)
		}

		if s.stopped != 1 {
			t.Errorf("%d sessions stopped", s.stopped)
		}

		// Nothing starts again.
		time.Sleep(time.Hour)
		synctest.Wait()

		if s.startCount() != 1 {
			t.Errorf("%d sessions started", s.startCount())
		}
	})
}

func TestStop_BeforeStartHasNothingToWaitFor(t *testing.T) {
	p, c := newTestPlugin(t, &sessions{})

	done := p.Stop()
	if _, open := <-done; open {
		t.Error("unexpected callback")
	}

	if len(c.deregistered) != 0 || len(c.receiversOff) != 0 {
		t.Errorf("deregistered %v %v", c.deregistered, c.receiversOff)
	}
}

func TestOnInterest_AnInterestThatIsNotValidIsIgnoredAndOtherEventsAreNotInterests(t *testing.T) {
	p, c := newTestPlugin(t, &sessions{})
	p.Start()

	defer stop(p)

	if err := c.receiver(&events.EventInfo{Event: bluetooth.InterestEvent{Owner: "bluetooth", Rules: []bluetooth.Rule{{}}}}); err != nil {
		t.Errorf("the receiver failed: %v", err)
	}

	if p.coalescer.Interested() {
		t.Error("an invalid interest was taken")
	}

	if err := c.receiver(&events.EventInfo{Event: "something else"}); err != nil {
		t.Error(err)
	}
}

func TestSession_ASessionBlueZEndsItselfIsAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &sessions{scripts: []sessionScript{{end: true}, {}}}
		p, _ := newTestPlugin(t, s)

		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		if err := p.session(ctx, "hci9", "bluez_1"); err == nil {
			t.Error("a session that ended early was not an error")
		}

		// One that runs its time is not.
		ctx2, cancel2 := context.WithTimeout(context.Background(), time.Minute)
		defer cancel2()

		if err := p.session(ctx2, "hci9", "bluez_1"); err != nil {
			t.Errorf("a session that ran its time: %v", err)
		}
	})
}
