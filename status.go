package bluez

import (
	"embed"
	"errors"
	"fmt"
	"github.com/avanha/pmaas-spi/format"
	"io"
	"net/http"
	"reflect"
	"sync"
	"text/template"
	"time"

	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/environment"
)

//go:embed content/static content/templates
var contentFS embed.FS

// staleAfter is how long a radio that is scanning may hear nothing before the page says it may have stopped. There is always
// something to hear in a house: a thermometer, a phone.
const staleAfter = 2 * time.Minute

var statusTemplate = spi.TemplateInfo{
	Name:    "bluez_status",
	FuncMap: template.FuncMap{"RelativeTime": format.AgoSeconds},
	Paths:   []string{"templates/bluez_status.htmlt"},
	Styles:  []string{"css/bluez_status.css"},
}

// diagnostics is what the scanning goroutine notes for the page: how many discovery sessions it has started, and the last
// time one could not be.
type diagnostics struct {
	mu            sync.Mutex
	adapter       string
	sessions      uint64
	lastError     string
	lastErrorTime time.Time
}

func (d *diagnostics) sessionStarted() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.sessions++
}

func (d *diagnostics) failed(err error, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.lastError, d.lastErrorTime = err.Error(), at
}

func (d *diagnostics) setAdapter(adapter string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.adapter = adapter
}

// status is the source as the page shows it.
type status struct {
	Name    string
	Adapter string

	// Connectivity is "online" when a discovery session is running, "offline" when one could not be started, and empty before the first
	// attempt. Since is when it became so.
	Connectivity string
	Since        time.Time

	Scanning string

	LastHeard time.Time
	Heard     uint64
	Forwarded uint64
	Sighted   uint64
	Dropped   uint64
	Sessions  uint64

	// Stale is that the radio is scanning and has heard nothing for a while, which is not a quiet house.
	Stale bool

	LastError     string
	LastErrorTime time.Time
}

var statusType = reflect.TypeFor[status]()

// status is what the page shows, as of now. It is safe to call from any goroutine.
func (p *plugin) status(now time.Time) status {
	s := p.source.GetSourceData()

	p.diag.mu.Lock()
	d := status{
		Name:          p.config.Name,
		Adapter:       p.diag.adapter,
		Sessions:      p.diag.sessions,
		LastError:     p.diag.lastError,
		LastErrorTime: p.diag.lastErrorTime,
	}
	p.diag.mu.Unlock()

	d.Scanning = s.Scanning.String()
	d.LastHeard, d.Heard, d.Forwarded, d.Sighted, d.Dropped = s.LastHeard, s.Heard, s.Forwarded, s.Sighted, s.Dropped

	switch s.Connectivity {
	case environment.ConnectivityOnline:
		d.Connectivity, d.Since = "online", s.OnlineSince
	case environment.ConnectivityOffline:
		d.Connectivity, d.Since = "offline", s.OfflineSince
	}

	if d.Connectivity == "online" {
		// Counted from the last thing heard, or if nothing has been, from when the session began.
		reference := d.LastHeard
		if reference.Before(d.Since) {
			reference = d.Since
		}

		d.Stale = now.Sub(reference) > staleAfter
	}

	return d
}

func (p *plugin) handleStatus(w http.ResponseWriter, r *http.Request) {
	view := p.status(time.Now())

	p.container.RenderList(w, r, spi.RenderListOptions{Title: "BlueZ"}, []any{&view})
}

func (p *plugin) statusRendererFactory() (spi.EntityRenderer, error) {
	compiled, err := p.container.GetTemplate(&statusTemplate)
	if err != nil {
		return spi.EntityRenderer{}, fmt.Errorf("unable to load bluez_status template: %v", err)
	}

	renderer := func(w io.Writer, entity any) error {
		view, ok := entity.(*status)
		if !ok {
			return errors.New("item is not an instance of *status")
		}

		if err := compiled.Instance.Execute(w, view); err != nil {
			return fmt.Errorf("unable to execute bluez_status template: %w", err)
		}

		return nil
	}

	return spi.EntityRenderer{StreamingRenderFunc: renderer, Styles: compiled.Styles, Scripts: compiled.Scripts}, nil
}
