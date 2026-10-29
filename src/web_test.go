package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dustin/go-broadcast"
	"github.com/gofiber/fiber/v3"
)

const webTestTimeout = 2 * time.Second

func newWebTestRequest(t *testing.T, id uint64, priority int64) *ActiveRequest {
	t.Helper()

	return &ActiveRequest{
		Id:       id,
		Priority: priority,
		Status:   RequestStatusPending,
		Url:      mustParseUrl(t, fmt.Sprintf("https://example.test/r%d", id)),
	}
}

func newWebTestSet(requests ...*ActiveRequest) *activeRequestSet {
	set := newActiveRequestSet()
	for _, request := range requests {
		set.add(request)
	}

	return set
}

// webGet performs one in-memory request; app.Test never opens a listener.
func webGet(t *testing.T, app *fiber.App, method string, path string) (int, string, string) {
	t.Helper()

	resp, err := app.Test(httptest.NewRequest(method, path, nil), fiber.TestConfig{Timeout: webTestTimeout, FailOnTimeout: true})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

func webWaitFor(t *testing.T, description string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(webTestTimeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}

		time.Sleep(time.Millisecond)
	}
}

func snapshotIds(set *activeRequestSet) []uint64 {
	ids := []uint64{}
	for _, item := range set.snapshot() {
		ids = append(ids, item.Id)
	}

	return ids
}

func pendingRows(body string) int {
	// One <tr> belongs to the table header.
	return strings.Count(body, "<tr>") - 1
}

func TestWebStaticRoutes(t *testing.T) {
	app := newWebRouter(newActiveRequestSet())

	for _, testCase := range []struct {
		name             string
		method           string
		path             string
		expectedStatus   int
		expectedHtml     bool
		expectedFragment string
	}{
		{"main page", http.MethodGet, "/", http.StatusOK, true, `hx-get="/pending"`},
		{"empty pending", http.MethodGet, "/pending", http.StatusOK, true, "No pending items"},
		{"unknown route", http.MethodGet, "/missing", http.StatusNotFound, false, ""},
		{"wrong method", http.MethodPost, "/", http.StatusMethodNotAllowed, false, ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, contentType, body := webGet(t, app, testCase.method, testCase.path)

			if status != testCase.expectedStatus {
				t.Fatalf("status = %d, want %d", status, testCase.expectedStatus)
			}

			if testCase.expectedHtml && !strings.HasPrefix(contentType, "text/html") {
				t.Fatalf("content type = %q", contentType)
			}

			if !strings.Contains(body, testCase.expectedFragment) {
				t.Fatalf("body misses %q:\n%s", testCase.expectedFragment, body)
			}
		})
	}
}

func TestWebPendingPage(t *testing.T) {
	escaped := newWebTestRequest(t, 1, 0)
	escaped.Url = mustParseUrl(t, "https://example.test/search?q=<b>")

	detailed := newWebTestRequest(t, 42, 9)
	detailed.Status = RequestStatusActive
	detailed.Retries = 7

	many := make([]*ActiveRequest, 150)
	for index := range many {
		many[index] = newWebTestRequest(t, uint64(index), 0)
	}

	for _, testCase := range []struct {
		name              string
		requests          []*ActiveRequest
		expectedRows      int
		expectedOrdered   []string
		unexpectedContent []string
	}{
		{
			name:              "empty",
			expectedRows:      -1,
			expectedOrdered:   []string{"No pending items"},
			unexpectedContent: []string{"Total"},
		},
		{
			name:            "request details",
			requests:        []*ActiveRequest{detailed},
			expectedRows:    1,
			expectedOrdered: []string{"Total 1 pending requests", ">42</td>", ">9</td>", ">1</td>", ">7</td>", "https://example.test/r42"},
		},
		{
			name: "priority descending then id ascending",
			requests: []*ActiveRequest{
				newWebTestRequest(t, 3, 1),
				newWebTestRequest(t, 2, 5),
				newWebTestRequest(t, 1, 5),
				newWebTestRequest(t, 0, -1),
			},
			expectedRows:    4,
			expectedOrdered: []string{"/r1<", "/r2<", "/r3<", "/r0<"},
		},
		{
			name:              "html escaping",
			requests:          []*ActiveRequest{escaped},
			expectedRows:      1,
			expectedOrdered:   []string{"q=&lt;b&gt;"},
			unexpectedContent: []string{"<b>"},
		},
		{
			name:              "first hundred rendered with full total",
			requests:          many,
			expectedRows:      maxPendingItems,
			expectedOrdered:   []string{"Total 150 pending requests", "/r0<", "/r99<"},
			unexpectedContent: []string{"/r100<", "/r149<"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, body := webGet(t, newWebRouter(newWebTestSet(testCase.requests...)), http.MethodGet, "/pending")

			if rows := pendingRows(body); rows != testCase.expectedRows {
				t.Fatalf("rows = %d, want %d", rows, testCase.expectedRows)
			}

			position := 0
			for _, fragment := range testCase.expectedOrdered {
				index := strings.Index(body[position:], fragment)
				if index < 0 {
					t.Fatalf("missing %q after offset %d:\n%s", fragment, position, body)
				}

				position += index + len(fragment)
			}

			for _, fragment := range testCase.unexpectedContent {
				if strings.Contains(body, fragment) {
					t.Fatalf("unexpected %q:\n%s", fragment, body)
				}
			}
		})
	}
}

func TestWebActiveRequestSet(t *testing.T) {
	first := newWebTestRequest(t, 1, 0)
	second := newWebTestRequest(t, 2, 0)

	for _, testCase := range []struct {
		name     string
		apply    func(set *activeRequestSet)
		expected []uint64
	}{
		{"duplicate add", func(set *activeRequestSet) { set.add(first); set.add(first) }, []uint64{1}},
		{"remove unknown", func(set *activeRequestSet) { set.add(first); set.remove(second) }, []uint64{1}},
		{"remove twice", func(set *activeRequestSet) { set.add(first); set.remove(first); set.remove(first) }, []uint64{}},
		{"re-add after remove", func(set *activeRequestSet) { set.add(first); set.remove(first); set.add(first) }, []uint64{1}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			set := newActiveRequestSet()
			testCase.apply(set)

			if ids := snapshotIds(set); !reflect.DeepEqual(ids, testCase.expected) {
				t.Fatalf("ids = %v, want %v", ids, testCase.expected)
			}
		})
	}
}

func TestWebSnapshotIsDetached(t *testing.T) {
	request := newWebTestRequest(t, 1, 0)
	set := newWebTestSet(request)
	before := set.snapshot()

	request.Lock.Lock()
	request.Status = RequestStatusFinished
	request.Retries = 3
	request.Lock.Unlock()

	if before[0].Status != RequestStatusPending || before[0].Retries != 0 {
		t.Fatalf("snapshot changed with request: %+v", before[0])
	}

	after := set.snapshot()
	if after[0].Status != RequestStatusFinished || after[0].Retries != 3 {
		t.Fatalf("new snapshot misses update: %+v", after[0])
	}
}

// Run with -race: request mutation, set mutation and rendering overlap.
func TestWebPendingConcurrentMutation(t *testing.T) {
	requests := make([]*ActiveRequest, 8)
	for index := range requests {
		requests[index] = newWebTestRequest(t, uint64(index), int64(index%3))
	}

	set := newWebTestSet(requests...)
	app := newWebRouter(set)

	var group sync.WaitGroup
	for _, request := range requests {
		group.Add(1)
		go func() {
			defer group.Done()

			for iteration := range 200 {
				request.Lock.Lock()
				request.Status = RequestStatus(iteration % 3)
				request.Retries++
				request.Lock.Unlock()

				if iteration%2 == 0 {
					set.remove(request)
				} else {
					set.add(request)
				}
			}
		}()
	}

	for range 20 {
		if status, _, _ := webGet(t, app, http.MethodGet, "/pending"); status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
	}

	group.Wait()
}

// startWebTestWatcher returns broadcasters whose cleanup waits for watcher shutdown first.
func startWebTestWatcher(t *testing.T) (*activeRequestSet, broadcast.Broadcaster, broadcast.Broadcaster, context.CancelFunc, <-chan struct{}) {
	t.Helper()

	requests := broadcast.NewBroadcaster(1)
	finished := broadcast.NewBroadcaster(1)
	t.Cleanup(func() {
		requests.Close()
		finished.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	set := newActiveRequestSet()
	done := watchActiveRequests(ctx, set, requests, finished)
	t.Cleanup(func() {
		cancel()
		waitClosed(t, done)
	})

	return set, requests, finished, cancel, done
}

func waitClosed(t *testing.T, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(webTestTimeout):
		t.Fatal("watcher did not stop")
	}
}

func TestWebWatcherEvents(t *testing.T) {
	type step struct {
		finished bool
		id       uint64
		expected []uint64
	}

	for _, testCase := range []struct {
		name  string
		steps []step
	}{
		{"new then finished", []step{{false, 1, []uint64{1}}, {true, 1, []uint64{}}}},
		{"finish one of two", []step{{false, 1, []uint64{1}}, {false, 2, []uint64{1, 2}}, {true, 1, []uint64{2}}}},
		{"request resubmitted after finish", []step{{false, 1, []uint64{1}}, {true, 1, []uint64{}}, {false, 1, []uint64{1}}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			set, requests, finished, _, _ := startWebTestWatcher(t)
			known := map[uint64]*ActiveRequest{}

			for _, step := range testCase.steps {
				request, ok := known[step.id]
				if !ok {
					request = newWebTestRequest(t, step.id, 0)
					known[step.id] = request
				}

				if step.finished {
					finished.Submit(request)
				} else {
					requests.Submit(request)
				}

				webWaitFor(t, fmt.Sprintf("ids %v", step.expected), func() bool {
					return reflect.DeepEqual(snapshotIds(set), step.expected)
				})
			}
		})
	}
}

func TestWebWatcherShutdown(t *testing.T) {
	for _, testCase := range []struct {
		name               string
		deliverDuringClose bool
	}{
		{"idle", false},
		{"while events are delivered", true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			set, requests, finished, cancel, done := startWebTestWatcher(t)
			request := newWebTestRequest(t, 1, 0)

			stopDelivery := make(chan struct{})
			var delivery sync.WaitGroup
			if testCase.deliverDuringClose {
				delivery.Add(1)
				go func() {
					defer delivery.Done()

					for {
						select {
						case <-stopDelivery:
							return
						default:
							requests.Submit(request)
							finished.Submit(request)
						}
					}
				}()
			}

			cancel()
			waitClosed(t, done)
			close(stopDelivery)
			delivery.Wait()

			before := snapshotIds(set)
			late := newWebTestRequest(t, 2, 0)

			// Unregistered channels are no longer fed, so submissions cannot block.
			submitted := make(chan struct{})
			go func() {
				for range 3 {
					requests.Submit(late)
					finished.Submit(request)
				}
				close(submitted)
			}()
			waitClosed(t, submitted)

			if after := snapshotIds(set); !reflect.DeepEqual(before, after) {
				t.Fatalf("set changed after shutdown: %v -> %v", before, after)
			}
		})
	}
}

func TestWebAppServesWatchedRequests(t *testing.T) {
	requests := broadcast.NewBroadcaster(1)
	finished := broadcast.NewBroadcaster(1)
	t.Cleanup(func() {
		requests.Close()
		finished.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	app, done := newWebApp(ctx, requests, finished)
	t.Cleanup(func() {
		cancel()
		waitClosed(t, done)
	})

	request := newWebTestRequest(t, 5, 0)
	pendingBody := func() string {
		_, _, body := webGet(t, app, http.MethodGet, "/pending")

		return body
	}

	requests.Submit(request)
	webWaitFor(t, "request row", func() bool { return strings.Contains(pendingBody(), "https://example.test/r5") })

	finished.Submit(request)
	webWaitFor(t, "empty page", func() bool { return strings.Contains(pendingBody(), "No pending items") })
}
