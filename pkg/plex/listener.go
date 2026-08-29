package plex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gorilla/websocket"
	"github.com/jrudio/go-plex-client/v2"
)

var (
	ErrAlreadyListening = errors.New("already listening")
)

type plexListener struct {
	server         *Server
	conn           *plex.Plex
	activeSessions *sessions
	log            log.Logger
}

func (s *Server) Listen(ctx context.Context, log log.Logger) error {
	s.mtx.Lock()
	if s.listener != nil {
		s.mtx.Unlock()
		return ErrAlreadyListening
	}

	conn, err := plex.New(s.URL.String(), s.Token)
	if err != nil {
		s.mtx.Unlock()
		return fmt.Errorf("failed to connect to %s: %w", s.URL.String(), err)
	}

	s.listener = &plexListener{
		server:         s,
		conn:           conn,
		activeSessions: NewSessions(ctx, s),
		log:            log,
	}

	s.mtx.Unlock()

	// forward context completion to jrudio/go-plex-client
	ctrlC := make(chan os.Signal, 1)
	go func() {
		<-ctx.Done()
		close(ctrlC)
	}()

	doneChan := make(chan error, 1)
	onError := func(err error) {
		defer close(doneChan)
		var closeErr *websocket.CloseError
		if errors.As(err, &closeErr) {
			if closeErr.Code == websocket.CloseNormalClosure {
				return
			}
		}
		level.Error(log).Log("msg", "error in websocket processing", "err", err)
		doneChan <- err
	}

	events := plex.NewNotificationEvents()
	events.OnPlaying(s.listener.onPlayingHandler)

	// Before subscribing, so the first notification for a live session (Plex sends one every
	// ~10s) finds the entry already seeded and Update simply continues it.
	s.listener.backfillSessions()

	// TODO - Does this automatically reconnect on websocket failure?
	conn.SubscribeToNotifications(events, ctrlC, onError)
	select { // SubscribeToNotifications doesn't return error directly, so we read one from channel without blocking.
	case err = <-doneChan:
		return err
	default:
		// noop
	}

	level.Info(log).Log("msg", "Successfully connected", "machineID", s.ID, "server", s.Name)

	return <-doneChan
}

// backfillSessions credits the sessions Plex is already serving at startup. Update only
// stamps playStarted on a transition INTO playing, and a session that is already playing when
// the process starts never makes that transition — so without this a pod roll (one happens on
// every image bump) restarts every live session's play time from zero.
// ⚠ A session PAUSED at startup was worse than undercounted: playStarted stayed zero and
// Collect skips a zero playStarted outright, so it contributed nothing at all, ever.
// Best-effort — a failure here costs accuracy on in-flight sessions, never the listener.
func (l *plexListener) backfillSessions() {
	sessions, err := l.conn.GetSessions()
	if err != nil {
		level.Warn(l.log).Log("msg", "could not backfill in-flight sessions", "err", err)
		return
	}

	for _, s := range sessions.MediaContainer.Metadata {
		state := sessionState(s.Player.State)
		if state == stateStopped {
			continue
		}

		metadata, err := l.conn.GetMetadata(s.RatingKey)
		if err != nil {
			level.Warn(l.log).Log("msg", "skipping session in backfill", "SessionKey", s.SessionKey, "err", err)
			continue
		}
		// Same empty-container guard as onPlaying: an unguarded [0] here panics the process.
		if len(metadata.MediaContainer.Metadata) == 0 {
			level.Warn(l.log).Log("msg", "skipping session in backfill", "SessionKey", s.SessionKey, "err", "no metadata returned")
			continue
		}
		media := metadata.MediaContainer.Metadata[0]

		viewOffset := time.Duration(s.ViewOffset) * time.Millisecond
		level.Info(l.log).Log("msg", "Backfilling in-flight session",
			"SessionKey", s.SessionKey,
			"userName", s.User.Title,
			"state", state,
			"mediaTitle", media.Title,
			"mediaID", media.RatingKey,
			"timestamp", viewOffset)

		l.activeSessions.Seed(s.SessionKey, state, &s, &media, viewOffset)
	}
}

func getSessionByID(sessions plex.CurrentSessions, sessionID string) *plex.Metadata {
	for _, session := range sessions.MediaContainer.Metadata {
		if sessionID == session.SessionKey {
			return &session
		}
	}
	return nil
}

func (l *plexListener) onPlayingHandler(c plex.NotificationContainer) {
	err := l.onPlaying(c)
	if err != nil {
		level.Error(l.log).Log("msg", "error handling OnPlaying event", "event", c, "err", err)
	}
}

func (l *plexListener) onPlaying(c plex.NotificationContainer) error {
	sessions, err := l.conn.GetSessions()
	if err != nil {
		return fmt.Errorf("error fetching sessions: %w", err)
	}

	for _, n := range c.PlaySessionStateNotification {
		if sessionState(n.State) == stateStopped {
			// When the session is stopped we can't look up the user info or media anymore.
			l.activeSessions.Update(n.SessionKey, sessionState(n.State), nil, nil)
			continue
		}

		session := getSessionByID(sessions, n.SessionKey)
		if session == nil {
			return fmt.Errorf("error getting session with key %s %+v", n.SessionKey, n)
		}

		metadata, err := l.conn.GetMetadata(n.RatingKey)
		if err != nil {
			return fmt.Errorf("error fetching metadata for key %s: %w", n.RatingKey, err)
		}

		// Same class of bug as the Media[0] indexes in sessions.go, on the websocket
		// goroutine instead of the scrape path: an empty container panicked the process.
		if len(metadata.MediaContainer.Metadata) == 0 {
			return fmt.Errorf("no metadata returned for key %s", n.RatingKey)
		}
		media := metadata.MediaContainer.Metadata[0]

		level.Info(l.log).Log("msg", "Received PlaySessionStateNotification",
			"SessionKey", n.SessionKey,
			"userName", session.User.Title,
			"userID", session.User.ID,
			"state", n.State,
			"mediaTitle", media.Title,
			"mediaID", media.RatingKey,
			"timestamp", time.Duration(time.Millisecond)*time.Duration(n.ViewOffset))

		l.activeSessions.Update(n.SessionKey, sessionState(n.State), session, &media)
	}

	return nil
}
