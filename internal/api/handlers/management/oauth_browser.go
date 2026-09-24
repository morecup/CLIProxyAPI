package management

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
)

const (
	oauthBrowserTicketTTL          = 30 * time.Second
	oauthBrowserSessionWait        = 20 * time.Second
	oauthBrowserTicketProtocolBase = "cliproxy-claude-ticket."
)

type oauthBrowserTicket struct {
	state     string
	expiresAt time.Time
}

type oauthBrowserTicketStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	tickets map[string]oauthBrowserTicket
}

func newOAuthBrowserTicketStore(ttl time.Duration) *oauthBrowserTicketStore {
	if ttl <= 0 {
		ttl = oauthBrowserTicketTTL
	}
	return &oauthBrowserTicketStore{ttl: ttl, tickets: make(map[string]oauthBrowserTicket)}
}

func (s *oauthBrowserTicketStore) issue(state string) (string, time.Duration, error) {
	if s == nil {
		return "", 0, errors.New("browser ticket store is unavailable")
	}
	state = strings.TrimSpace(state)
	if errState := ValidateOAuthState(state); errState != nil {
		return "", 0, errState
	}
	random := make([]byte, 32)
	if _, errRead := rand.Read(random); errRead != nil {
		return "", 0, fmt.Errorf("generate browser ticket: %w", errRead)
	}
	ticket := base64.RawURLEncoding.EncodeToString(random)
	now := time.Now()
	s.mu.Lock()
	s.purgeExpiredLocked(now)
	s.tickets[ticket] = oauthBrowserTicket{state: state, expiresAt: now.Add(s.ttl)}
	s.mu.Unlock()
	return ticket, s.ttl, nil
}

func (s *oauthBrowserTicketStore) consume(state, ticket string) bool {
	if s == nil {
		return false
	}
	state = strings.TrimSpace(state)
	ticket = strings.TrimSpace(ticket)
	if state == "" || ticket == "" {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	s.purgeExpiredLocked(now)
	entry, ok := s.tickets[ticket]
	if ok {
		delete(s.tickets, ticket)
	}
	s.mu.Unlock()
	if !ok || now.After(entry.expiresAt) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(entry.state), []byte(state)) == 1
}

func (s *oauthBrowserTicketStore) purgeExpiredLocked(now time.Time) {
	for ticket, entry := range s.tickets {
		if !entry.expiresAt.After(now) {
			delete(s.tickets, ticket)
		}
	}
}

var oauthBrowserTickets = newOAuthBrowserTicketStore(oauthBrowserTicketTTL)

// CreateOAuthBrowserTicket mints a one-time, short-lived WebSocket ticket for
// an already authenticated management session. The management password is
// never placed in a WebSocket URL or browser subprotocol.
func (h *Handler) CreateOAuthBrowserTicket(c *gin.Context) {
	state := strings.TrimSpace(c.Param("state"))
	if errState := ValidateOAuthState(state); errState != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}
	provider, status, isPlugin, _, completed, ok := GetOAuthSessionDetails(state)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "unknown or expired state"})
		return
	}
	if completed || status != "" || isPlugin || !strings.EqualFold(provider, "anthropic") || !IsOAuthSessionPending(state, "anthropic") {
		if status == "" {
			status = "Claude Desktop authentication is not waiting for browser verification"
		}
		c.JSON(http.StatusConflict, gin.H{"status": "error", "error": status})
		return
	}
	ticket, ttl, errTicket := oauthBrowserTickets.issue(state)
	if errTicket != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to create browser verification ticket"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":     "ok",
		"ticket":     ticket,
		"expires_in": int(ttl.Seconds()),
	})
}

// StreamOAuthBrowser upgrades a single-use ticket to an interactive stream of
// the isolated Claude verification page.
func (h *Handler) StreamOAuthBrowser(c *gin.Context) {
	state := strings.TrimSpace(c.Param("state"))
	if errState := ValidateOAuthState(state); errState != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}
	if !websocket.IsWebSocketUpgrade(c.Request) {
		c.JSON(http.StatusUpgradeRequired, gin.H{"status": "error", "error": "websocket upgrade required"})
		return
	}
	protocol, ticket := oauthBrowserTicketFromRequest(c.Request)
	if protocol == "" || !oauthBrowserTickets.consume(state, ticket) {
		c.JSON(http.StatusUnauthorized, gin.H{"status": "error", "error": "invalid or expired browser verification ticket"})
		return
	}
	if !IsOAuthSessionPending(state, "anthropic") {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "error": "Claude Desktop authentication is no longer pending"})
		return
	}

	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 256 << 10,
		Subprotocols:    []string{protocol},
		CheckOrigin: func(*http.Request) bool {
			// The one-time ticket was minted through the authenticated management
			// API and is consumed before the upgrade, so custom management UI
			// origins remain supported without exposing the management key.
			return true
		},
	}
	connection, errUpgrade := upgrader.Upgrade(c.Writer, c.Request, nil)
	if errUpgrade != nil {
		return
	}
	defer connection.Close()

	if errWrite := writeOAuthBrowserEvent(connection, claudedesktop.MagicLinkBrowserEvent{Type: "waiting"}); errWrite != nil {
		return
	}
	session, errSession := waitForOAuthBrowserSession(state, oauthBrowserSessionWait)
	if errSession != nil {
		_ = writeOAuthBrowserEvent(connection, claudedesktop.MagicLinkBrowserEvent{Type: "error", Message: errSession.Error()})
		return
	}
	events, unsubscribe, errSubscribe := session.Subscribe()
	if errSubscribe != nil {
		_ = writeOAuthBrowserEvent(connection, claudedesktop.MagicLinkBrowserEvent{Type: "error", Message: "Claude verification browser is unavailable"})
		return
	}
	defer unsubscribe()

	connection.SetReadLimit(4096)
	_ = connection.SetReadDeadline(time.Now().Add(45 * time.Second))
	connection.SetPongHandler(func(string) error {
		return connection.SetReadDeadline(time.Now().Add(45 * time.Second))
	})

	readDone := make(chan error, 1)
	inputErrors := make(chan error, 1)
	go func() {
		for {
			var event claudedesktop.MagicLinkBrowserInput
			if errRead := connection.ReadJSON(&event); errRead != nil {
				readDone <- errRead
				return
			}
			if errInput := session.DispatchInput(event); errInput != nil {
				select {
				case inputErrors <- errInput:
				default:
				}
			}
		}
	}()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case event, open := <-events:
			if !open {
				_ = writeOAuthBrowserEvent(connection, claudedesktop.MagicLinkBrowserEvent{Type: "closed"})
				return
			}
			if errWrite := writeOAuthBrowserEvent(connection, event); errWrite != nil {
				return
			}
		case <-session.Done():
			_ = writeOAuthBrowserEvent(connection, claudedesktop.MagicLinkBrowserEvent{Type: "closed", Message: session.CloseReason()})
			return
		case errInput := <-inputErrors:
			if errWrite := writeOAuthBrowserEvent(connection, claudedesktop.MagicLinkBrowserEvent{Type: "input_error", Message: errInput.Error()}); errWrite != nil {
				return
			}
		case <-readDone:
			return
		case <-ping.C:
			_ = connection.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if errPing := connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); errPing != nil {
				return
			}
		}
	}
}

func oauthBrowserTicketFromRequest(request *http.Request) (string, string) {
	if request == nil {
		return "", ""
	}
	for _, protocol := range websocket.Subprotocols(request) {
		protocol = strings.TrimSpace(protocol)
		if !strings.HasPrefix(protocol, oauthBrowserTicketProtocolBase) {
			continue
		}
		ticket := strings.TrimSpace(strings.TrimPrefix(protocol, oauthBrowserTicketProtocolBase))
		if ticket == "" || len(ticket) > 128 {
			return "", ""
		}
		return protocol, ticket
	}
	return "", ""
}

func waitForOAuthBrowserSession(state string, timeout time.Duration) (*claudedesktop.MagicLinkBrowserSession, error) {
	if timeout <= 0 {
		timeout = oauthBrowserSessionWait
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if session, ok := claudedesktop.GetMagicLinkBrowserSession(state); ok {
			return session, nil
		}
		if !IsOAuthSessionPending(state, "anthropic") {
			_, status, ok := GetOAuthSession(state)
			if ok && strings.TrimSpace(status) != "" {
				return nil, errors.New(strings.TrimSpace(status))
			}
			return nil, errors.New("Claude Desktop authentication is no longer pending")
		}
		select {
		case <-timer.C:
			return nil, errors.New("Claude verification browser did not become ready")
		case <-ticker.C:
		}
	}
}

func writeOAuthBrowserEvent(connection *websocket.Conn, event claudedesktop.MagicLinkBrowserEvent) error {
	if connection == nil {
		return errors.New("websocket connection is unavailable")
	}
	_ = connection.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return connection.WriteJSON(event)
}
