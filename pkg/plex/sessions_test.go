package plex

import (
	"encoding/json"
	"testing"
	"time"

	plex "github.com/jrudio/go-plex-client"
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

func playingSession(episode string) session {
	media := plex.Metadata{
		LibrarySectionID: json.Number(libID),
		Type:             "episode",
		GrandparentTitle: "Some Series",
		ParentTitle:      "Season 1",
		Title:            episode,
		Media:            []plex.Media{{VideoResolution: "1080"}},
	}
	live := media
	live.Media = []plex.Media{{
		Bitrate:         3652,
		VideoResolution: "1080",
		Part:            []plex.Part{{Decision: "directplay"}},
	}}
	live.Player = plex.Player{Device: "tvOS", Product: "Plex for Apple TV"}
	live.User = plex.User{Title: "someone"}

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
	s := &sessions{sessions: ss, server: testServer()}
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
