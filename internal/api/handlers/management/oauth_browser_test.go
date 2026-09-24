package management

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestOAuthBrowserTicketStoreIsSingleUseAndStateBound(t *testing.T) {
	store := newOAuthBrowserTicketStore(time.Minute)
	ticket, ttl, errIssue := store.issue("state-one")
	if errIssue != nil {
		t.Fatalf("issue() error = %v", errIssue)
	}
	if ticket == "" || ttl != time.Minute {
		t.Fatalf("issue() = (%q, %s), want a one-minute ticket", ticket, ttl)
	}
	if store.consume("state-two", ticket) {
		t.Fatal("consume() accepted a ticket for a different state")
	}
	if store.consume("state-one", ticket) {
		t.Fatal("consume() reused a ticket after a failed state match")
	}

	ticket, _, errIssue = store.issue("state-one")
	if errIssue != nil {
		t.Fatalf("second issue() error = %v", errIssue)
	}
	if !store.consume("state-one", ticket) {
		t.Fatal("consume() rejected a valid ticket")
	}
	if store.consume("state-one", ticket) {
		t.Fatal("consume() accepted a reused ticket")
	}
}

func TestCreateOAuthBrowserTicketRequiresPendingAnthropicSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalSessions := oauthSessions
	originalTickets := oauthBrowserTickets
	oauthSessions = newOAuthSessionStore(time.Minute)
	oauthBrowserTickets = newOAuthBrowserTicketStore(time.Minute)
	t.Cleanup(func() {
		oauthSessions = originalSessions
		oauthBrowserTickets = originalTickets
	})

	oauthSessions.Register("browser-state", "anthropic")
	router := gin.New()
	handler := &Handler{}
	router.POST("/oauth-session/:state/browser-ticket", handler.CreateOAuthBrowserTicket)

	request := httptest.NewRequest("POST", "/oauth-session/browser-state/browser-ticket", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("pending anthropic response = %d: %s", response.Code, response.Body.String())
	}

	oauthSessions.SetError("browser-state", "failed")
	request = httptest.NewRequest("POST", "/oauth-session/browser-state/browser-ticket", nil)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 409 {
		t.Fatalf("errored anthropic response = %d, want 409", response.Code)
	}
}
