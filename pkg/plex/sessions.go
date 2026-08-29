package plex

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/jrudio/go-plex-client/v2"
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

	// How long a session that Plex told us had STOPPED is kept before it is retired.
	sessionTimeout = time.Minute

	// How long a session that never stopped is kept after its last notification. Plex sends
	// one every ~10s per session (measured 2026-08-29), so this is ~30 missed notifications:
	// it fires only when a client vanished without a stop — network drop, app killed — and
	// never on a brief websocket reconnect. Without it such a session is immortal, and a
	// still-`playing` one keeps accruing play time for a stream that ended hours ago.
	sessionStaleTimeout = 5 * time.Minute
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
	mtx      sync.Mutex
	sessions map[string]session
	server   *Server

	// completed banks the totals of sessions that have left `sessions`, so plays_total and
	// play_seconds_total keep the cumulative meaning their _total suffix and CounterValue
	// type promise. Without it a series simply VANISHES when its session is pruned, which
	// Prometheus reads as a counter reset — increase() then re-adds the whole value.
	// ⚠ It is never pruned: that is what makes the counter monotonic. It therefore grows
	// with the number of distinct label tuples the process has seen (~900 over 90d
	// historically), which is the memory and scrape-size cost of the guarantee.
	completed map[playKey]*playAgg

	totalEstimatedTransmittedKBits float64
}

func NewSessions(ctx context.Context, server *Server) *sessions {
	s := &sessions{
		sessions:  map[string]session{},
		completed: map[playKey]*playAgg{},
		server:    server,
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
		timeout := sessionStaleTimeout
		if v.state == stateStopped {
			timeout = sessionTimeout
		}
		if time.Since(v.lastUpdate) > timeout {
			s.retire(v)
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

// observedPlayTime is the play time we have actually SEEN for a session. While playing, the
// current interval is credited only up to lastUpdate, not to now: Plex sends a notification
// every ~10s, so anything past the last one is unobserved. That is what lets retire() bank
// exactly the value Collect was already emitting, so the counter cannot step backwards when
// a session is pruned.
func (ss session) observedPlayTime() time.Duration {
	if ss.state != statePlaying {
		return ss.prevPlayedTime
	}
	return ss.prevPlayedTime + ss.lastUpdate.Sub(ss.playStarted)
}

// retire banks a departing session's totals into `completed`. Caller holds the mutex and must
// delete the session afterwards; banking twice would double-count it.
func (s *sessions) retire(ss session) {
	k, ok := s.playKeyFor(ss)
	if !ok {
		return
	}

	if s.completed == nil {
		s.completed = map[playKey]*playAgg{}
	}
	agg, ok := s.completed[k]
	if !ok {
		agg = &playAgg{}
		s.completed[k] = agg
	}
	agg.plays++
	agg.seconds += ss.observedPlayTime().Seconds()

	// A session retired while still playing never reached Update's flatten branch, so its
	// estimated bytes would otherwise vanish with it and drop that counter too.
	if ss.state == statePlaying {
		played := ss.lastUpdate.Sub(ss.playStarted)
		s.totalEstimatedTransmittedKBits += played.Seconds() * float64(bitrate(ss.session))
	}
}

func (s *sessions) extrapolatedTransmittedBytes() float64 {

	total := s.totalEstimatedTransmittedKBits

	for _, ss := range s.sessions {
		if ss.state == statePlaying {
			// lastUpdate, not now — same reason as observedPlayTime: retire() banks this
			// exact quantity, so using now here would make the counter drop on a prune.
			total += ss.lastUpdate.Sub(ss.playStarted).Seconds() * float64(bitrate(ss.session))
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

// playKeyFor builds the exported label tuple for a session. Not ok when the session has never
// been observed playing, or when its library has not been resolved yet — in both cases there
// is nothing meaningful to key it by, and Collect and retire must agree on that.
func (s *sessions) playKeyFor(ss session) (playKey, bool) {
	if ss.playStarted.IsZero() {
		return playKey{}, false
	}
	library := s.server.Library(ss.media.LibrarySectionID.String())
	if library == nil {
		return playKey{}, false
	}
	title, season, episode := labels(ss.media)

	return playKey{
		library:     library.Name,
		libraryID:   library.ID,
		libraryType: library.Type,
		mediaType:   ss.media.Type,
		title:       title,
		season:      season,
		episode:     episode,
		streamType:  streamDecision(ss.session),
		streamRes:   videoResolution(ss.session),
		fileRes:     videoResolution(ss.media),
		bitrate:     strconv.Itoa(bitrate(ss.session)),
		device:      ss.session.Player.Device,
		deviceType:  ss.session.Player.Product,
		user:        ss.session.User.Title,
	}, true
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
	//
	// Emitted value = what retired sessions banked + what the live ones have run up so far.
	totals := make(map[playKey]playAgg, len(s.completed)+len(s.sessions))
	for k, agg := range s.completed {
		totals[k] = *agg
	}

	for _, session := range s.sessions {
		k, ok := s.playKeyFor(session)
		if !ok {
			continue
		}
		agg := totals[k]
		agg.plays++
		agg.seconds += session.observedPlayTime().Seconds()
		totals[k] = agg
	}

	for k, agg := range totals {
		// plays_total counts PLAYS, cumulatively: every session this process has seen on
		// this tuple, retired ones included. It was hardcoded 1.0 per session before v2.0.0,
		// which only worked because the `session` label split them.
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
