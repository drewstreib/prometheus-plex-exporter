package plex

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/jrudio/go-plex-client"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/drewstreib/prometheus-plex-exporter/pkg/metrics"
)

type sessionState string

const (
	statePlaying   sessionState = "playing"
	stateStopped   sessionState = "stopped"
	statePaused    sessionState = "paused"
	stateBuffering sessionState = "buffering"

	mediaTypeEpisode = "episode"

	// How long metrics for sessions are kept after the last update.
	// This is used to prune prometheus metrics and keep cardinality
	// down.
	sessionTimeout = time.Minute
)

type session struct {
	session        plex.Metadata
	media          plex.Metadata
	state          sessionState
	lastUpdate     time.Time
	playStarted    time.Time
	prevPlayedTime time.Duration
}

type sessions struct {
	mtx                            sync.Mutex
	sessions                       map[string]session
	server                         *Server
	totalEstimatedTransmittedKBits float64
}

func NewSessions(ctx context.Context, server *Server) *sessions {
	s := &sessions{
		sessions: map[string]session{},
		server:   server,
	}

	ticker := time.NewTicker(time.Minute)
	go func() {
		for {
			select {
			case <-ticker.C:
				s.pruneOldSessions()
			case <-ctx.Done():
				ticker.Stop()
				return
			}
		}
	}()

	return s
}

func (s *sessions) pruneOldSessions() {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	for k, v := range s.sessions {
		if v.state == stateStopped && time.Since(v.lastUpdate) > sessionTimeout {
			delete(s.sessions, k)
		}
	}
}

func (s *sessions) Update(sessionID string, newState sessionState, newSession *plex.Metadata, media *plex.Metadata) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	ss := s.sessions[sessionID]

	if newSession != nil {
		ss.session = *newSession
	}

	if media != nil {
		ss.media = *media
	}

	if ss.state == statePlaying && newState != statePlaying {
		// If the session was playing but now is not, then flatten
		// the play time into the total.
		ss.prevPlayedTime += time.Since(ss.playStarted)
		s.totalEstimatedTransmittedKBits += time.Since(ss.playStarted).Seconds() * float64(bitrate(ss.session))
	}

	if ss.state != statePlaying && newState == statePlaying {
		// Started playing
		ss.playStarted = time.Now()
	}

	ss.state = newState
	ss.lastUpdate = time.Now()
	s.sessions[sessionID] = ss
}

// Seed credits a session that was already in flight when the process started, backfilling
// the play time it accrued before we existed from the session's ViewOffset.
//
// ⚠ STARTUP ONLY — never seed on first sight of an unknown session key. Plex assigns a NEW
// SessionKey to a resume, so unknown keys routinely arrive mid-file: measured 2026-08-29,
// key 1308 sat paused at 3m11s and key 1310 then appeared *playing* at that same 3m11s.
// Seeding per unknown key would credit the whole resume offset as play time on every resume;
// at startup the same offset is the only elapsed-time signal there is and the error is one-shot.
func (s *sessions) Seed(sessionID string, state sessionState, live, media *plex.Metadata, viewOffset time.Duration) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	if _, known := s.sessions[sessionID]; known {
		// A notification beat the backfill to it; that path has the real transition and wins.
		return
	}

	now := time.Now()
	ss := session{
		session:     *live,
		media:       *media,
		state:       state,
		lastUpdate:  now,
		playStarted: now.Add(-viewOffset),
	}
	if state != statePlaying {
		// Nothing accrues from playStarted unless the session is playing, so bank the offset
		// directly. playStarted is still set, because Collect skips a zero one outright.
		ss.prevPlayedTime = viewOffset
	}
	s.sessions[sessionID] = ss
}

func (s *sessions) extrapolatedTransmittedBytes() float64 {

	total := s.totalEstimatedTransmittedKBits

	for _, ss := range s.sessions {
		if ss.state == statePlaying {
			total += time.Since(ss.playStarted).Seconds() * float64(bitrate(ss.session))
		}
	}

	return total * 128.0 // Kbits -> Bytes, 1024 / 8
}

func (s *sessions) Describe(ch chan<- *prometheus.Desc) {
	ch <- metrics.MetricPlayCountDesc
	ch <- metrics.MetricPlaySecondsTotalDesc

	ch <- metrics.MetricEstimatedTransmittedBytesTotal
}

// playKey is the exported label tuple for plays_total / play_seconds_total, minus the
// server labels. It replaces the old per-session `session` label.
type playKey struct {
	library, libraryID, libraryType         string
	mediaType, title, season, episode       string
	streamType, streamRes, fileRes, bitrate string
	device, deviceType, user                string
}

type playAgg struct {
	plays   float64
	seconds float64
}

func (s *sessions) Collect(ch chan<- prometheus.Metric) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	// ⚠ AGGREGATE BEFORE EMITTING. The `session` label used to make every series unique by
	// construction; it was dropped 2026-08-29 because it bought nothing analytically (and
	// only ~34% of the cardinality — per-episode titles are the real driver). Without it,
	// two sessions sharing a tuple — the same user restarting the same episode while the
	// stopped session is still inside the 1-minute prune window — would emit DUPLICATE
	// label sets, and client_golang fails the ENTIRE scrape on a duplicate, not just the
	// offending metric. So sessions must be folded here, never emitted one-per-session.
	totals := map[playKey]*playAgg{}

	for _, session := range s.sessions {
		if session.playStarted.IsZero() {
			continue
		}

		title, season, episode := labels(session.media)
		library := s.server.Library(session.media.LibrarySectionID.String())
		if library == nil {
			continue
		}

		k := playKey{
			library:     library.Name,
			libraryID:   library.ID,
			libraryType: library.Type,
			mediaType:   session.media.Type,
			title:       title,
			season:      season,
			episode:     episode,
			streamType:  streamDecision(session.session),
			streamRes:   videoResolution(session.session),
			fileRes:     videoResolution(session.media),
			bitrate:     strconv.Itoa(bitrate(session.session)),
			device:      session.session.Player.Device,
			deviceType:  session.session.Player.Product,
			user:        session.session.User.Title,
		}

		totalPlayTime := session.prevPlayedTime
		if session.state == statePlaying {
			totalPlayTime += time.Since(session.playStarted)
		}

		agg, ok := totals[k]
		if !ok {
			agg = &playAgg{}
			totals[k] = agg
		}
		agg.plays++
		agg.seconds += totalPlayTime.Seconds()
	}

	for k, agg := range totals {
		// plays_total is now the NUMBER of concurrent sessions on this tuple; it was
		// hardcoded 1.0 per session before, which only worked because `session` split them.
		ch <- metrics.Play(
			agg.plays,
			"plex", s.server.Name, s.server.ID,
			k.library, k.libraryID, k.libraryType,
			k.mediaType, k.title, k.season, k.episode,
			k.streamType, k.streamRes, k.fileRes, k.bitrate,
			k.device, k.deviceType, k.user,
		)
		ch <- metrics.PlayDuration(
			agg.seconds,
			"plex", s.server.Name, s.server.ID,
			k.library, k.libraryID, k.libraryType,
			k.mediaType, k.title, k.season, k.episode,
			k.streamType, k.streamRes, k.fileRes, k.bitrate,
			k.device, k.deviceType, k.user,
		)
	}

	ch <- prometheus.MustNewConstMetric(metrics.MetricEstimatedTransmittedBytesTotal, prometheus.CounterValue, s.extrapolatedTransmittedBytes(), "plex", s.server.Name,
		s.server.ID)
}

// ⚠ Plex reports sessions with NO Media (and Media with no Part) — live TV, transient
// buffering states, and the stop/cleanup race. Upstream indexes [0] unguarded inside
// Collect, which is FATAL: client_golang carries no recover() anywhere, and Gather runs
// Collect on its own goroutine, so one such session kills the exporter on the next scrape
// rather than returning a 500. These accessors degrade to a zero value instead.
func bitrate(m plex.Metadata) int {
	if len(m.Media) == 0 {
		return 0
	}
	return m.Media[0].Bitrate
}

func videoResolution(m plex.Metadata) string {
	if len(m.Media) == 0 {
		return ""
	}
	return m.Media[0].VideoResolution
}

func streamDecision(m plex.Metadata) string {
	if len(m.Media) == 0 || len(m.Media[0].Part) == 0 {
		return ""
	}
	return m.Media[0].Part[0].Decision
}

func labels(m plex.Metadata) (title, season, episodeTitle string) {
	if m.Type == mediaTypeEpisode {
		return m.GrandparentTitle, m.ParentTitle, m.Title
	}
	return m.Title, "", ""
}
