package plex

import (
	"encoding/json"
	"testing"
	"time"

	plex "github.com/jrudio/go-plex-client/v2"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// The library the fixtures below live in. Name/ID/Type are deliberately all different
// strings so a rotation cannot pass by coincidence.
const (
	libName = "TV Shows"
	libID   = "2"
	libType = "show"
)

func testServer() *Server {
	return &Server{
		ID:        "srv-id",
		Name:      "srv-name",
		libraries: []*Library{{ID: libID, Name: libName, Type: libType}},
	}
}

// sessionMetadata returns the (live session, library media) pair the listener hands to
// Update and Seed.
func sessionMetadata(episode string) (live, media plex.Metadata) {
	media = plex.Metadata{
		LibrarySectionID: json.Number(libID),
		Type:             "episode",
		GrandparentTitle: "Some Series",
		ParentTitle:      "Season 1",
		Title:            episode,
		Media:            []plex.Media{{VideoResolution: "1080"}},
	}
	live = media
	live.Media = []plex.Media{{
		Bitrate:         3652,
		VideoResolution: "1080",
		Part:            []plex.Part{{Decision: "directplay"}},
	}}
	live.Player = plex.Player{Device: "tvOS", Product: "Plex for Apple TV"}
	live.User = plex.User{Title: "someone"}
	return live, media
}

func playingSession(episode string) session {
	live, media := sessionMetadata(episode)

	return session{
		session:     live,
		media:       media,
		state:       statePlaying,
		playStarted: time.Now().Add(-10 * time.Second),
		lastUpdate:  time.Now(),
	}
}

// gather registers the collector for real and scrapes it, so a duplicate label set fails
// here exactly as it would fail a Prometheus scrape.
func gather(t *testing.T, ss map[string]session) []*dto.MetricFamily {
	t.Helper()
	return gatherFrom(t, &sessions{sessions: ss, server: testServer()})
}

func gatherFrom(t *testing.T, s *sessions) []*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(s)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() failed — this is a broken scrape, not a warning: %v", err)
	}
	return mfs
}

func family(t *testing.T, mfs []*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	t.Fatalf("metric family %q not collected", name)
	return nil
}

func labelOf(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// Upstream passed (library, libraryID, libraryType) into label slots ordered
// (library_type, library, library_id), rotating all three: `library` carried the id,
// `library_id` the type, `library_type` the name. library_duration_total was correct, so
// the two metrics could not be joined. Regression guard for that fix.
func TestCollectLibraryLabelsAreNotRotated(t *testing.T) {
	mfs := gather(t, map[string]session{"s1": playingSession("Ep 1")})

	for _, name := range []string{"plays_total", "play_seconds_total"} {
		mf := family(t, mfs, name)
		if len(mf.GetMetric()) != 1 {
			t.Fatalf("%s: got %d series, want 1", name, len(mf.GetMetric()))
		}
		m := mf.GetMetric()[0]
		for _, tc := range []struct{ label, want string }{
			{"library", libName},
			{"library_id", libID},
			{"library_type", libType},
		} {
			if got := labelOf(m, tc.label); got != tc.want {
				t.Errorf("%s{%s} = %q, want %q", name, tc.label, got, tc.want)
			}
		}
	}
}

// The `session` label was dropped, so two sessions sharing a tuple now collide. They must
// be folded into one series — client_golang fails the WHOLE scrape on a duplicate.
func TestCollectAggregatesSessionsSharingATuple(t *testing.T) {
	mfs := gather(t, map[string]session{
		"s1": playingSession("Ep 1"),
		"s2": playingSession("Ep 1"),
	})

	mf := family(t, mfs, "plays_total")
	if len(mf.GetMetric()) != 1 {
		t.Fatalf("got %d series, want 1 aggregated series", len(mf.GetMetric()))
	}
	if got := mf.GetMetric()[0].GetCounter().GetValue(); got != 2 {
		t.Errorf("plays_total = %v, want 2 (one per folded session)", got)
	}
	if l := labelOf(mf.GetMetric()[0], "session"); l != "" {
		t.Errorf("session label still exported: %q", l)
	}

	secs := family(t, mfs, "play_seconds_total").GetMetric()[0].GetCounter().GetValue()
	if secs < 15 {
		t.Errorf("play_seconds_total = %v, want the SUM of both sessions (~20)", secs)
	}
}

// Distinct episodes must stay distinct.
func TestCollectKeepsDistinctTuplesSeparate(t *testing.T) {
	mfs := gather(t, map[string]session{
		"s1": playingSession("Ep 1"),
		"s2": playingSession("Ep 2"),
	})
	if n := len(family(t, mfs, "plays_total").GetMetric()); n != 2 {
		t.Errorf("got %d series, want 2", n)
	}
}

// Plex returns sessions with no Media / no Part. Upstream indexed [0] unguarded, and
// because client_golang has no recover() and Gather runs Collect on its own goroutine,
// that panic killed the process on the next scrape rather than returning a 500.
func TestCollectSurvivesSessionsWithNoMedia(t *testing.T) {
	bare := playingSession("Ep 1")
	bare.session.Media = nil
	bare.media.Media = nil

	noPart := playingSession("Ep 2")
	noPart.session.Media = []plex.Media{{Bitrate: 100, VideoResolution: "720"}} // no Part

	mfs := gather(t, map[string]session{"s1": bare, "s2": noPart})
	if n := len(family(t, mfs, "plays_total").GetMetric()); n != 2 {
		t.Errorf("got %d series, want 2", n)
	}
}

// --- startup backfill (Seed) ---------------------------------------------------------
//
// Update stamps playStarted only on a transition INTO playing, so a session already in
// flight when the process starts never gets one. Seed is the startup backfill for that.

const seedOffset = 10 * time.Minute

func emptySessions() *sessions {
	return &sessions{sessions: map[string]session{}, server: testServer()}
}

// seconds asserts exactly one play_seconds_total series and returns its value.
func seconds(t *testing.T, mfs []*dto.MetricFamily) float64 {
	t.Helper()
	mf := family(t, mfs, "play_seconds_total")
	if len(mf.GetMetric()) != 1 {
		t.Fatalf("play_seconds_total: got %d series, want 1", len(mf.GetMetric()))
	}
	return mf.GetMetric()[0].GetCounter().GetValue()
}

// A session PLAYING at startup keeps the time it accrued before the process existed.
func TestSeedPlayingBackfillsElapsedTime(t *testing.T) {
	s := emptySessions()
	live, media := sessionMetadata("Ep 1")
	s.Seed("1310", statePlaying, &live, &media, seedOffset)

	if got := seconds(t, gatherFrom(t, s)); got < 600 || got > 605 {
		t.Errorf("play_seconds_total = %v, want ~600 (the backfilled ViewOffset)", got)
	}
}

// A session PAUSED at startup used to be dropped entirely, not merely undercounted:
// playStarted stayed zero and Collect skips a zero playStarted outright.
func TestSeedPausedIsCountedNotSkipped(t *testing.T) {
	s := emptySessions()
	live, media := sessionMetadata("Ep 1")
	s.Seed("1308", statePaused, &live, &media, seedOffset)

	if got := seconds(t, gatherFrom(t, s)); got < 600 || got > 605 {
		t.Errorf("play_seconds_total = %v, want ~600 — a paused session must still be collected", got)
	}
}

// Pausing flattens time.Since(playStarted) into prevPlayedTime. Since Seed backdates
// playStarted rather than pre-filling prevPlayedTime, that must not count the offset twice.
func TestSeedPlayingDoesNotDoubleCountOnPause(t *testing.T) {
	s := emptySessions()
	live, media := sessionMetadata("Ep 1")
	s.Seed("1310", statePlaying, &live, &media, seedOffset)
	s.Update("1310", statePaused, &live, &media)

	if got := seconds(t, gatherFrom(t, s)); got < 600 || got > 605 {
		t.Errorf("play_seconds_total = %v, want ~600 not ~1200 (offset counted twice)", got)
	}
}

// The mirror case: a paused seed banks the offset in prevPlayedTime, so resuming must add
// to it rather than restart from it.
func TestSeedPausedResumesOnTopOfTheBankedOffset(t *testing.T) {
	s := emptySessions()
	live, media := sessionMetadata("Ep 1")
	s.Seed("1308", statePaused, &live, &media, seedOffset)
	s.Update("1308", statePlaying, &live, &media)

	if got := seconds(t, gatherFrom(t, s)); got < 600 || got > 605 {
		t.Errorf("play_seconds_total = %v, want ~600 plus the moment since resume", got)
	}
}

// Plex sends a notification every ~10s, so one can land before the backfill finishes. The
// notification has the real transition; the backfill must not overwrite it.
func TestSeedDoesNotOverwriteAKnownSession(t *testing.T) {
	s := emptySessions()
	live, media := sessionMetadata("Ep 1")
	s.Update("1310", statePlaying, &live, &media)
	s.Seed("1310", statePlaying, &live, &media, seedOffset)

	if got := seconds(t, gatherFrom(t, s)); got > 5 {
		t.Errorf("play_seconds_total = %v, want ~0 — Seed clobbered a live session's timing", got)
	}
}

// --- cumulative counters and stale-session retirement ---------------------------------
//
// plays_total / play_seconds_total are declared CounterValue and named _total, so they must
// be cumulative and monotonic. They used to be rebuilt from the LIVE session map on every
// scrape, so a series vanished when its session was pruned — which Prometheus reads as a
// counter reset, and increase() then re-adds the whole value.

// ageSession backdates a session's last-notification time, standing in for wall-clock passing.
func ageSession(s *sessions, key string, lastSeenAgo time.Duration) {
	ss := s.sessions[key]
	ss.lastUpdate = time.Now().Add(-lastSeenAgo)
	s.sessions[key] = ss
}

// Pruning a stopped session must bank its totals, not discard them.
func TestPruneBanksTotalsSoTheCounterDoesNotDrop(t *testing.T) {
	s := emptySessions()
	live, media := sessionMetadata("Ep 1")
	s.Seed("1310", statePlaying, &live, &media, seedOffset)
	s.Update("1310", stateStopped, &live, &media)

	before := seconds(t, gatherFrom(t, s))
	if before < 600 {
		t.Fatalf("fixture banked only %v seconds; the test could not detect a drop", before)
	}

	ageSession(s, "1310", 2*sessionTimeout)
	s.pruneOldSessions()

	if n := len(s.sessions); n != 0 {
		t.Fatalf("stopped session not pruned: %d left", n)
	}
	if after := seconds(t, gatherFrom(t, s)); after < before {
		t.Errorf("play_seconds_total dropped %v -> %v when the session was pruned", before, after)
	}
}

// A client that vanishes (network drop, app killed) never sends `stopped`. Pruning only
// stopped sessions left those immortal, and a still-`playing` one accrued play time forever.
func TestPruneRetiresAStaleSessionThatNeverStopped(t *testing.T) {
	s := emptySessions()
	live, media := sessionMetadata("Ep 1")
	s.Seed("1310", statePlaying, &live, &media, 16*time.Minute)
	ageSession(s, "1310", 6*time.Minute) // Plex went quiet, state still `playing`

	before := seconds(t, gatherFrom(t, s))
	s.pruneOldSessions()

	if n := len(s.sessions); n != 0 {
		t.Fatalf("a session that never stopped was kept: %d left", n)
	}
	if after := seconds(t, gatherFrom(t, s)); after < before {
		t.Errorf("play_seconds_total dropped %v -> %v when the stale session was retired", before, after)
	}
}

// Credit stops at the last notification. Plex sends one every ~10s, so time after that is
// unobserved — counting it would invent play time for a stream that may have ended.
func TestPlayTimeStopsAtTheLastNotification(t *testing.T) {
	s := emptySessions()
	live, media := sessionMetadata("Ep 1")
	s.Seed("1310", statePlaying, &live, &media, 16*time.Minute)
	ageSession(s, "1310", 6*time.Minute)

	if got := seconds(t, gatherFrom(t, s)); got < 600 || got > 605 {
		t.Errorf("play_seconds_total = %v, want ~600 (10m observed), not ~960 (16m of wall clock)", got)
	}
}

// Successive plays of the same episode must add up rather than replace one another.
func TestPlaysAccumulateAcrossRetiredSessions(t *testing.T) {
	s := emptySessions()
	live, media := sessionMetadata("Ep 1")

	for _, key := range []string{"1310", "1330"} { // Plex issues a new SessionKey per play
		s.Seed(key, statePlaying, &live, &media, seedOffset)
		s.Update(key, stateStopped, &live, &media)
		ageSession(s, key, 2*sessionTimeout)
		s.pruneOldSessions()
	}

	mf := family(t, gatherFrom(t, s), "plays_total")
	if len(mf.GetMetric()) != 1 {
		t.Fatalf("plays_total: got %d series, want 1", len(mf.GetMetric()))
	}
	if got := mf.GetMetric()[0].GetCounter().GetValue(); got != 2 {
		t.Errorf("plays_total = %v, want 2 — a retired play must stay counted", got)
	}
}

// estimated_transmit_bytes_total is flattened by Update on a playing->not-playing transition.
// A session retired while still playing never makes that transition, so retire must flatten it.
func TestEstimatedBytesSurviveRetiringAPlayingSession(t *testing.T) {
	s := emptySessions()
	live, media := sessionMetadata("Ep 1")
	s.Seed("1310", statePlaying, &live, &media, 16*time.Minute)
	ageSession(s, "1310", 6*time.Minute)

	before := s.extrapolatedTransmittedBytes()
	if before == 0 {
		t.Fatal("fixture produced 0 bytes; the test could not detect a drop")
	}

	s.pruneOldSessions()

	if after := s.extrapolatedTransmittedBytes(); after != before {
		t.Errorf("estimated_transmit_bytes_total = %v after retiring a playing session, want %v", after, before)
	}
}
