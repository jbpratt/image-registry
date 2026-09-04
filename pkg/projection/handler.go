package projection

import (
	"encoding/json"
	"net/http"

	"github.com/distribution/distribution/v3/notifications"
)

// envelope mirrors notifications.Envelope but decodes each event into the
// concrete notifications.Event struct (the upstream Envelope holds the
// go-events.Event interface, which JSON cannot populate).
type envelope struct {
	Events []notifications.Event `json:"events"`
}

// Handler returns the notification intake. It decodes distribution's event
// envelope and, for push/delete events with a repository, enqueues only the repo
// key. No event data reaches reconcile (level-triggered, fact 5).
func (c *Controller) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env envelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received := c.now()
		for i := range env.Events {
			ev := &env.Events[i]
			if ev.Action != notifications.EventActionPush && ev.Action != notifications.EventActionDelete {
				continue
			}
			if ev.Target.Repository == "" {
				continue
			}
			if c.dropFunc != nil && c.dropFunc() {
				eventsDroppedTotal.Inc()
				continue
			}
			c.enqueue(ev.Target.Repository, received)
		}
		w.WriteHeader(http.StatusOK)
	})
}
