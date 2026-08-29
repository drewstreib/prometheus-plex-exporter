package plex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/gorilla/websocket"
)

// fakePlex serves just enough for Listen: it upgrades the notifications websocket and hands
// the connection to onConn, and 404s everything else (backfillSessions is best-effort by
// design, so a failing /status/sessions must not change the outcome under test).
func fakePlex(t *testing.T, onConn func(*websocket.Conn)) *Server {
	t.Helper()

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/:/websockets/notifications" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		onConn(conn)
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	return &Server{ID: "srv-id", Name: "srv-name", URL: u, Token: "token"}
}

// listenResult runs Listen in the background and returns the channel it reports on.
func listenResult(t *testing.T, s *Server) <-chan error {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- s.Listen(ctx, log.NewNopLogger()) }()
	return done
}

// TestListenReturnsOnAbnormalWebsocketClose pins the answer to upstream's unanswered
// "TODO - Does this automatically reconnect on websocket failure?": it does NOT. The
// library dials once, and on a read error unwinds all the way out of
// SubscribeToNotifications. Listen must therefore RETURN A NON-NIL ERROR, because main
// turns that into exit 1 and the pod restart IS the reconnect. If this ever starts
// hanging instead, the exporter is alive with a dead socket and up{job="plex"}=1 while
// no play metric can ever move again.
func TestListenReturnsOnAbnormalWebsocketClose(t *testing.T) {
	// Slam the TCP connection with no close frame: a Plex restart or a dropped link.
	s := fakePlex(t, func(conn *websocket.Conn) { conn.UnderlyingConn().Close() })

	select {
	case err := <-listenResult(t, s):
		if err == nil {
			t.Fatal("Listen returned nil after an abnormal close; main would exit 0 as if it had been asked to stop")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Listen did not return within 15s of the websocket dying: it neither reconnects nor exits")
	}
}

// TestListenReturnsOnServerInitiatedNormalClose covers the other half. A close(1000) is
// filtered out of doneChan by onError, which leaves the channel CLOSED rather than
// carrying an error, so Listen returns nil and main exits 0. Still not a reconnect —
// still a restart — but the exit code differs, which is what a restart-reason check reads.
func TestListenReturnsOnServerInitiatedNormalClose(t *testing.T) {
	s := fakePlex(t, func(conn *websocket.Conn) {
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		conn.Close()
	})

	select {
	case err := <-listenResult(t, s):
		if err != nil {
			t.Logf("normal close surfaced as an error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Listen did not return within 15s of a normal close")
	}
}
