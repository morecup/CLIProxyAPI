package tasks

import "github.com/google/uuid"

// The outbox belongs to the protected task store. A new query may inspect an
// old query's unresolved obligations but must not send them under its epoch.
type delivery struct {
	Sequence uint64 `json:"sequence"`
	Scope    string `json:"scope"`
	Event    Event  `json:"event"`
	Notify   bool   `json:"notify"`
	Observed bool   `json:"observed"`
}

func (r *Runtime) validateOutbox() error {
	var previous uint64
	ids := map[string]bool{}
	for _, item := range r.outbox {
		t := r.tasks[item.Event.TaskID]
		if item.Sequence <= previous || item.Sequence > r.nextEvent || t == nil ||
			item.Event.Generation == 0 || item.Event.Generation > t.Generation || ids[item.Event.ID] ||
			(item.Event.Kind != "started" && item.Event.Kind != "finished") {
			return ErrInvalid
		}
		if id, err := uuid.Parse(item.Event.ID); err != nil || id == uuid.Nil {
			return ErrInvalid
		}
		ids[item.Event.ID] = true
		previous = item.Sequence
	}
	return nil
}

func (r *Runtime) enqueueEventLocked(t *task, kind string, notify bool) {
	r.nextEvent++
	r.outbox = append(r.outbox, delivery{Sequence: r.nextEvent, Scope: r.options.DeliveryScope,
		Event: eventOf(t, kind, r.options.Now()), Notify: notify})
}

func (r *Runtime) wakeDeliveryLocked() {
	select {
	case r.deliveryWake <- struct{}{}:
	default:
	}
}

// One dispatcher preserves transition order without holding the registry lock
// across callbacks or network I/O. Failure retains the obligation and a durable
// health flag; there is no timer-based network deadline or unbounded retry loop.
func (r *Runtime) deliver() {
	defer close(r.deliveryDone)
	for {
		select {
		case <-r.deliveryStop:
			return
		case <-r.deliveryWake:
		}
		for r.deliverOne() {
		}
	}
}

func (r *Runtime) deliverOne() bool {
	r.mu.Lock()
	index := -1
	for i, item := range r.outbox {
		if item.Scope == r.options.DeliveryScope {
			index = i
			break
		}
	}
	if index < 0 {
		r.mu.Unlock()
		return false
	}
	item := r.outbox[index]
	// Commit a previously failed terminal save before emitting its event.
	if err := r.saveLocked(); err != nil {
		r.tasks[item.Event.TaskID].persistenceFailed = true
		r.mu.Unlock()
		return false
	}
	r.mu.Unlock()

	err := r.ctx.Err()
	if err == nil && !item.Observed && r.options.Observe != nil {
		err = r.options.Observe(r.ctx, item.Event)
	}
	if err == nil && item.Observed && item.Notify && r.options.Notify != nil {
		err = r.options.Notify(item.Event)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tasks[item.Event.TaskID]
	if err != nil {
		t.DeliveryFailed = true
		if errSave := r.saveLocked(); errSave != nil {
			t.persistenceFailed = true
		}
		return false
	}
	// Preserve the remote/local delivery stages independently. A local enqueue
	// failure must not make a successfully delivered worker event appear unsent.
	previous := append([]delivery(nil), r.outbox...)
	if !item.Observed && item.Notify {
		r.outbox[index].Observed = true
	} else {
		r.outbox = append(r.outbox[:index], r.outbox[index+1:]...)
	}
	if err := r.saveLocked(); err != nil {
		r.outbox = previous
		t.persistenceFailed, t.DeliveryFailed = true, true
		return false
	}
	return true
}
