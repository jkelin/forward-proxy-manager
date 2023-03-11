package main

import (
	_ "embed"
	"html/template"
	"sort"
	"sync"
)
import (
	"context"
	"github.com/gofiber/fiber/v2"
)

//go:embed templates/main.html
var templateMainString string

//go:embed templates/pending.html
var templatePendingString string

type PendingTemplateData struct {
	Items []*ActiveRequest
	Total int
}

func runWeb(ctx context.Context) {
	app := fiber.New()

	activeRequests := make(map[*ActiveRequest]struct{})
	activeRequestsLock := sync.Mutex{}

	go func() {
		newRequests := make(chan interface{})
		newRequestsBroacast.Register(newRequests)
		defer newRequestsBroacast.Unregister(newRequests)

		finishedRequests := make(chan interface{})
		requestFinishedBroacast.Register(finishedRequests)
		defer requestFinishedBroacast.Unregister(finishedRequests)

		for {
			select {
			case <-ctx.Done():
				return
			case request := <-newRequests:
				activeRequestsLock.Lock()
				activeRequests[request.(*ActiveRequest)] = struct{}{}
				activeRequestsLock.Unlock()
			case request := <-finishedRequests:
				activeRequestsLock.Lock()
				delete(activeRequests, request.(*ActiveRequest))
				activeRequestsLock.Unlock()
			}
		}
	}()

	templateMain, err := template.New("foo").Parse(templateMainString)
	if err != nil {
		panic(err)
	}

	templatePending, err := template.New("foo").Parse(templatePendingString)
	if err != nil {
		panic(err)
	}

	app.Get("/", func(c *fiber.Ctx) error {
		c.Context().SetContentType("text/html")

		err := templateMain.Execute(c, nil)
		if err != nil {
			return err
		}

		return nil
	})

	app.Get("/pending", func(c *fiber.Ctx) error {
		activeRequestsLock.Lock()
		defer activeRequestsLock.Unlock()

		items := make([]*ActiveRequest, 0, len(activeRequests))

		for item := range activeRequests {
			items = append(items, item)
		}

		sort.Slice(items, func(i, j int) bool {
			if items[i].Priority != items[j].Priority {
				return items[i].Priority > items[j].Priority
			}

			return items[i].Id < items[j].Id
		})

		c.Context().SetContentType("text/html")

		data := PendingTemplateData{
			Total: len(items),
			Items: items,
		}

		if len(data.Items) > 100 {
			data.Items = data.Items[:100]
		}

		err := templatePending.Execute(c, data)
		if err != nil {
			return err
		}

		return nil
	})

	err = app.Listen(":8081")
	if err != nil {
		panic(err)
	}
}
