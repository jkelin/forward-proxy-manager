package main

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"log"
	"net/url"
	"sort"
	"sync"

	"github.com/dustin/go-broadcast"
	"github.com/gofiber/fiber/v3"
)

// maxPendingItems caps rendered rows; Total still counts every active request.
const maxPendingItems = 100

//go:embed templates/main.html
var templateMainString string

//go:embed templates/pending.html
var templatePendingString string

var (
	templateMain    = template.Must(template.New("main").Parse(templateMainString))
	templatePending = template.Must(template.New("pending").Parse(templatePendingString))
)

// PendingRequest is a detached copy of the ActiveRequest fields shown on /pending.
type PendingRequest struct {
	Id       uint64
	Priority int64
	Status   RequestStatus
	Retries  uint32
	Url      *url.URL
}

type PendingTemplateData struct {
	Items []PendingRequest
	Total int
}

// activeRequestSet tracks requests between their submit and finish broadcasts.
type activeRequestSet struct {
	lock     sync.Mutex
	requests map[*ActiveRequest]struct{}
}

func newActiveRequestSet() *activeRequestSet {
	return &activeRequestSet{requests: make(map[*ActiveRequest]struct{})}
}

// add is idempotent: resubmitted requests stay listed once.
func (set *activeRequestSet) add(request *ActiveRequest) {
	set.lock.Lock()
	defer set.lock.Unlock()

	set.requests[request] = struct{}{}
}

// remove ignores unknown requests.
func (set *activeRequestSet) remove(request *ActiveRequest) {
	set.lock.Lock()
	defer set.lock.Unlock()

	delete(set.requests, request)
}

// snapshot copies the displayed fields, highest priority first, then lowest id.
//
// Edge cases: the set lock is released before any request lock is taken, because
// request owners may broadcast while holding request.Lock and the watcher needs the
// set lock to receive. A request may finish between both steps; it is then rendered
// once more with its latest status.
func (set *activeRequestSet) snapshot() []PendingRequest {
	set.lock.Lock()
	requests := make([]*ActiveRequest, 0, len(set.requests))
	for request := range set.requests {
		requests = append(requests, request)
	}
	set.lock.Unlock()

	items := make([]PendingRequest, 0, len(requests))
	for _, request := range requests {
		request.Lock.Lock()
		items = append(items, PendingRequest{
			Id:       request.Id,
			Priority: request.Priority,
			Status:   request.Status,
			Retries:  request.Retries,
			Url:      request.Url,
		})
		request.Lock.Unlock()
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Priority != items[j].Priority {
			return items[i].Priority > items[j].Priority
		}

		return items[i].Id < items[j].Id
	})

	return items
}

// watchActiveRequests mirrors broadcast events into set until ctx is done.
// The returned channel closes once both channels are unregistered.
//
// Edge cases:
//   - Channels are registered before returning, so later submissions are observed.
//   - Events on the two broadcasters are not ordered relative to each other.
//   - Unregistering drains the channel, so a broadcaster blocked on delivery to
//     this watcher cannot deadlock shutdown.
//   - Events other than *ActiveRequest panic, as they violate the broadcast contract.
func watchActiveRequests(ctx context.Context, set *activeRequestSet, requests broadcast.Broadcaster, finished broadcast.Broadcaster) <-chan struct{} {
	requestEvents := make(chan interface{})
	finishedEvents := make(chan interface{})
	requests.Register(requestEvents)
	finished.Register(finishedEvents)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer unregisterDraining(finished, finishedEvents)
		defer unregisterDraining(requests, requestEvents)

		for {
			select {
			case <-ctx.Done():
				return
			case request := <-requestEvents:
				set.add(request.(*ActiveRequest))
			case request := <-finishedEvents:
				set.remove(request.(*ActiveRequest))
			}
		}
	}()

	return done
}

// unregisterDraining discards events while Unregister waits for the broadcaster loop.
func unregisterDraining(broadcaster broadcast.Broadcaster, events chan interface{}) {
	stop := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)

		for {
			select {
			case <-events:
			case <-stop:
				return
			}
		}
	}()

	broadcaster.Unregister(events)
	close(stop)
	<-drained
}

// newWebRouter serves the dashboard pages from activeRequests.
func newWebRouter(activeRequests *activeRequestSet) *fiber.App {
	app := fiber.New()

	app.Get("/", func(c fiber.Ctx) error {
		c.RequestCtx().SetContentType("text/html")

		return templateMain.Execute(c, nil)
	})

	app.Get("/pending", func(c fiber.Ctx) error {
		items := activeRequests.snapshot()
		data := PendingTemplateData{
			Total: len(items),
			Items: items[:min(len(items), maxPendingItems)],
		}

		c.RequestCtx().SetContentType("text/html")

		return templatePending.Execute(c, data)
	})

	return app
}

// newWebApp builds the dashboard without listening. The returned channel closes
// after ctx is done and the active-request watcher has unregistered.
func newWebApp(ctx context.Context, requests broadcast.Broadcaster, finished broadcast.Broadcaster) (*fiber.App, <-chan struct{}) {
	activeRequests := newActiveRequestSet()
	watcherDone := watchActiveRequests(ctx, activeRequests, requests, finished)

	return newWebRouter(activeRequests), watcherDone
}

// runWeb serves the dashboard on port until ctx is done, then stops gracefully.
func runWeb(ctx context.Context, scheduler *RequestScheduler, port uint16) {
	app, _ := newWebApp(ctx, scheduler.requests, scheduler.finished)

	err := app.Listen(fmt.Sprintf(":%d", port), fiber.ListenConfig{GracefulContext: ctx})
	if err != nil {
		log.Fatalf("Failed to start web server: %v", err)
	}
}
