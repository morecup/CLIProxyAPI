package claudedesktop

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
)

const (
	// InteractiveSessionMetadataKey lets management callers bind the temporary
	// Chromium page to an already authenticated management OAuth session.
	InteractiveSessionMetadataKey = "claude-desktop-interactive-session"

	maxMagicLinkBrowserFrameSize = 4 << 20
)

var errMagicLinkBrowserSessionUnavailable = errors.New("Claude Desktop verification browser is unavailable")

// MagicLinkBrowserEvent is streamed to the management page while a temporary
// Claude verification browser is active. Frame data is held in memory only.
type MagicLinkBrowserEvent struct {
	Type    string  `json:"type"`
	FrameID uint64  `json:"frame_id,omitempty"`
	Data    string  `json:"data,omitempty"`
	Width   float64 `json:"width,omitempty"`
	Height  float64 `json:"height,omitempty"`
	Message string  `json:"message,omitempty"`
}

// MagicLinkBrowserInput represents the deliberately small input surface
// exposed to the management page. Text input and arbitrary navigation are not
// supported; hCaptcha can be completed with pointer events alone.
type MagicLinkBrowserInput struct {
	Type       string  `json:"type"`
	Action     string  `json:"action"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	DeltaX     float64 `json:"delta_x,omitempty"`
	DeltaY     float64 `json:"delta_y,omitempty"`
	ClickCount int64   `json:"click_count,omitempty"`
}

type magicLinkBrowserDispatch func(MagicLinkBrowserInput) error

// MagicLinkBrowserSession is an in-memory bridge to one isolated Chromium
// target. It never persists screenshots or browser input.
type MagicLinkBrowserSession struct {
	dispatch magicLinkBrowserDispatch
	cancel   context.CancelFunc

	inputMu sync.Mutex
	mu      sync.RWMutex
	ready   bool
	closed  bool
	reason  string
	width   float64
	height  float64
	nextID  uint64
	latest  MagicLinkBrowserEvent
	done    chan struct{}
	nextSub uint64
	subs    map[uint64]chan MagicLinkBrowserEvent
}

var magicLinkBrowserRegistry = struct {
	sync.RWMutex
	sessions map[string]*MagicLinkBrowserSession
}{sessions: make(map[string]*MagicLinkBrowserSession)}

func registerMagicLinkBrowserSession(state string, cancel context.CancelFunc, dispatch magicLinkBrowserDispatch) *MagicLinkBrowserSession {
	state = strings.TrimSpace(state)
	if state == "" || dispatch == nil {
		return nil
	}
	session := &MagicLinkBrowserSession{
		dispatch: dispatch,
		cancel:   cancel,
		done:     make(chan struct{}),
		subs:     make(map[uint64]chan MagicLinkBrowserEvent),
	}

	magicLinkBrowserRegistry.Lock()
	previous := magicLinkBrowserRegistry.sessions[state]
	magicLinkBrowserRegistry.sessions[state] = session
	magicLinkBrowserRegistry.Unlock()
	if previous != nil {
		previous.stop("A newer Claude verification browser replaced this session")
	}
	return session
}

func unregisterMagicLinkBrowserSession(state string, session *MagicLinkBrowserSession) {
	if session == nil {
		return
	}
	magicLinkBrowserRegistry.Lock()
	if magicLinkBrowserRegistry.sessions[strings.TrimSpace(state)] == session {
		delete(magicLinkBrowserRegistry.sessions, strings.TrimSpace(state))
	}
	magicLinkBrowserRegistry.Unlock()
	session.close("")
}

// GetMagicLinkBrowserSession returns the live browser bound to state.
func GetMagicLinkBrowserSession(state string) (*MagicLinkBrowserSession, bool) {
	magicLinkBrowserRegistry.RLock()
	session := magicLinkBrowserRegistry.sessions[strings.TrimSpace(state)]
	magicLinkBrowserRegistry.RUnlock()
	if session == nil || session.isClosed() {
		return nil, false
	}
	return session, true
}

// CancelMagicLinkBrowserSession stops and removes the browser bound to state.
func CancelMagicLinkBrowserSession(state string) bool {
	state = strings.TrimSpace(state)
	magicLinkBrowserRegistry.Lock()
	session := magicLinkBrowserRegistry.sessions[state]
	if session != nil {
		delete(magicLinkBrowserRegistry.sessions, state)
	}
	magicLinkBrowserRegistry.Unlock()
	if session == nil {
		return false
	}
	session.stop("Claude verification was cancelled")
	return true
}

func (s *MagicLinkBrowserSession) isClosed() bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	return closed
}

func (s *MagicLinkBrowserSession) markReady(width, height float64) {
	if s == nil || !validBrowserCoordinate(width) || !validBrowserCoordinate(height) || width <= 0 || height <= 0 {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.ready = true
	s.width = width
	s.height = height
	event := MagicLinkBrowserEvent{Type: "ready", Width: width, Height: height}
	s.publishLocked(event)
	s.mu.Unlock()
}

func (s *MagicLinkBrowserSession) publishFrame(data string, width, height float64) {
	if s == nil || data == "" || len(data) > maxMagicLinkBrowserFrameSize {
		return
	}
	if !validBrowserCoordinate(width) || width <= 0 {
		width = 900
	}
	if !validBrowserCoordinate(height) || height <= 0 {
		height = 700
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.ready = true
	s.width = width
	s.height = height
	s.nextID++
	s.latest = MagicLinkBrowserEvent{
		Type:    "frame",
		FrameID: s.nextID,
		Data:    data,
		Width:   width,
		Height:  height,
	}
	s.publishLocked(s.latest)
	s.mu.Unlock()
}

func (s *MagicLinkBrowserSession) publishLocked(event MagicLinkBrowserEvent) {
	for _, ch := range s.subs {
		select {
		case ch <- event:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- event:
			default:
			}
		}
	}
}

// Subscribe returns a small latest-frame stream. Slow clients drop stale
// frames rather than increasing memory use.
func (s *MagicLinkBrowserSession) Subscribe() (<-chan MagicLinkBrowserEvent, func(), error) {
	if s == nil {
		return nil, nil, errMagicLinkBrowserSessionUnavailable
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, errMagicLinkBrowserSessionUnavailable
	}
	s.nextSub++
	id := s.nextSub
	ch := make(chan MagicLinkBrowserEvent, 3)
	s.subs[id] = ch
	if s.ready {
		ch <- MagicLinkBrowserEvent{Type: "ready", Width: s.width, Height: s.height}
	}
	if s.latest.FrameID != 0 {
		ch <- s.latest
	}
	s.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			s.mu.Lock()
			if existing, ok := s.subs[id]; ok {
				delete(s.subs, id)
				close(existing)
			}
			s.mu.Unlock()
		})
	}
	return ch, unsubscribe, nil
}

// Done closes when Chromium leaves the verification flow or the login ends.
func (s *MagicLinkBrowserSession) Done() <-chan struct{} {
	if s == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return s.done
}

// CloseReason returns a user-safe reason for a browser shutdown, when present.
func (s *MagicLinkBrowserSession) CloseReason() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	reason := s.reason
	s.mu.RUnlock()
	return reason
}

// DispatchInput sends a validated pointer event to Chromium.
func (s *MagicLinkBrowserSession) DispatchInput(event MagicLinkBrowserInput) error {
	if s == nil || s.isClosed() {
		return errMagicLinkBrowserSessionUnavailable
	}
	event.Type = strings.ToLower(strings.TrimSpace(event.Type))
	event.Action = strings.ToLower(strings.TrimSpace(event.Action))
	if event.Type != "mouse" {
		return fmt.Errorf("%w: unsupported input type", errMagicLinkBrowserSessionUnavailable)
	}
	if !validBrowserCoordinate(event.X) || !validBrowserCoordinate(event.Y) {
		return fmt.Errorf("%w: invalid pointer coordinates", errMagicLinkBrowserSessionUnavailable)
	}
	s.mu.RLock()
	width, height, ready := s.width, s.height, s.ready
	s.mu.RUnlock()
	if !ready || event.X < 0 || event.Y < 0 || event.X > width || event.Y > height {
		return fmt.Errorf("%w: pointer is outside the browser viewport", errMagicLinkBrowserSessionUnavailable)
	}
	switch event.Action {
	case "move", "down", "up":
	case "wheel":
		if !validBrowserDelta(event.DeltaX) || !validBrowserDelta(event.DeltaY) {
			return fmt.Errorf("%w: invalid wheel delta", errMagicLinkBrowserSessionUnavailable)
		}
	default:
		return fmt.Errorf("%w: unsupported pointer action", errMagicLinkBrowserSessionUnavailable)
	}
	if event.ClickCount < 0 || event.ClickCount > 2 {
		return fmt.Errorf("%w: invalid click count", errMagicLinkBrowserSessionUnavailable)
	}

	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	if s.isClosed() {
		return errMagicLinkBrowserSessionUnavailable
	}
	return s.dispatch(event)
}

func (s *MagicLinkBrowserSession) abort(reason string) {
	if s == nil {
		return
	}
	s.stop(strings.TrimSpace(reason))
}

func (s *MagicLinkBrowserSession) stop(reason string) {
	if s == nil {
		return
	}
	s.close(reason)
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *MagicLinkBrowserSession) close(reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.reason = strings.TrimSpace(reason)
	s.latest = MagicLinkBrowserEvent{}
	close(s.done)
	for id, ch := range s.subs {
		delete(s.subs, id)
		close(ch)
	}
	s.mu.Unlock()
}

func validBrowserCoordinate(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value <= 10000
}

func validBrowserDelta(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && math.Abs(value) <= 2000
}
