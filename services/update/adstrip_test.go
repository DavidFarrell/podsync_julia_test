package update

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	itunes "github.com/eduncan911/podcast"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/model"
)

// fakeStore records divert/job writes and serves canned results, so barrier tests
// never touch real disk or a real clock.
type fakeStore struct {
	diverted map[string][]byte // srcPath -> raw bytes
	jobs     map[string][]byte // jobPath -> job json
	results  map[string][]byte // resultPath -> result json
	outputs  map[string]string // outputPath -> contents

	divertErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		diverted: map[string][]byte{},
		jobs:     map[string][]byte{},
		results:  map[string][]byte{},
		outputs:  map[string]string{},
	}
}

func (f *fakeStore) DivertRaw(r io.Reader, srcPath string) error {
	if f.divertErr != nil {
		return f.divertErr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.diverted[srcPath] = b
	return nil
}

func (f *fakeStore) WriteJob(jobPath string, job []byte) error {
	f.jobs[jobPath] = job
	return nil
}

func (f *fakeStore) ReadResult(resultPath string) ([]byte, error) {
	b, ok := f.results[resultPath]
	if !ok {
		// Mimic os.ReadFile's not-exist so reconcile's os.IsNotExist branch fires.
		return nil, os.ErrNotExist
	}
	return b, nil
}

func (f *fakeStore) OpenOutput(outputPath string) (io.ReadCloser, error) {
	s, ok := f.outputs[outputPath]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(s)), nil
}

func (f *fakeStore) RemoveResult(resultPath string) error {
	delete(f.results, resultPath)
	return nil
}

type fakeClock struct{ now time.Time }

func (c fakeClock) Now() time.Time { return c.now }

func newTestBarrier(store adstripStore, now time.Time, timeout time.Duration) *adstripBarrier {
	return &adstripBarrier{
		cfg:   adstripConfig{Root: "/srv/adstrip", Timeout: timeout},
		store: store,
		clock: fakeClock{now: now},
	}
}

func TestSafePathComponent(t *testing.T) {
	bad := []string{"", ".", "..", ".hidden", "a/b", "x\x00y"}
	for _, name := range bad {
		_, err := safePathComponent(name)
		assert.Error(t, err, "expected %q to be rejected", name)
	}

	good := []string{"This Week in Tech", "abc123XYZ", "TWiT 1088"}
	for _, name := range good {
		got, err := safePathComponent(name)
		require.NoError(t, err, "expected %q to be allowed", name)
		assert.Equal(t, name, got)
	}
}

func TestResolvePathsRejectsTraversal(t *testing.T) {
	b := newTestBarrier(newFakeStore(), time.Now(), defaultAdstripTimeout)

	_, err := b.resolvePaths("../escape", "abc")
	assert.Error(t, err)

	_, err = b.resolvePaths("feed", "../../etc/passwd")
	assert.Error(t, err)

	paths, err := b.resolvePaths("This Week in Tech", "abc123")
	require.NoError(t, err)
	assert.Equal(t, "/srv/adstrip/adstrip-work/This Week in Tech/abc123.mp4", paths.src)
	assert.Equal(t, "/srv/adstrip/adstrip-work/This Week in Tech/abc123.cut.mp4", paths.cut)
	assert.Equal(t, "/srv/adstrip/adstrip-state/jobs/This Week in Tech/abc123.json", paths.job)
	assert.Equal(t, "/srv/adstrip/adstrip-state/results/This Week in Tech/abc123.json", paths.result)
}

func TestDivertWritesStagingAndJob(t *testing.T) {
	store := newFakeStore()
	now := time.Date(2026, 6, 21, 22, 40, 0, 0, time.UTC)
	b := newTestBarrier(store, now, defaultAdstripTimeout)

	cfg := &feed.Config{ID: "This Week in Tech"}
	cfg.StripAds = feed.StripAds{Enabled: true, Aggressiveness: feed.AggressivenessAggressive}

	raw := bytes.NewBufferString("RAW MP4 BYTES")
	require.NoError(t, b.divert(cfg, "abc123XYZ", "TWiT 1088", raw))

	srcPath := "/srv/adstrip/adstrip-work/This Week in Tech/abc123XYZ.mp4"
	jobPath := "/srv/adstrip/adstrip-state/jobs/This Week in Tech/abc123XYZ.json"

	// Raw landed in the UNSERVED staging tree, not a served path.
	require.Equal(t, "RAW MP4 BYTES", string(store.diverted[srcPath]))

	// Job JSON matches the contract.
	require.Contains(t, store.jobs, jobPath)
	var job adstripJob
	require.NoError(t, json.Unmarshal(store.jobs[jobPath], &job))
	assert.Equal(t, jobSchema, job.Schema)
	assert.Equal(t, "This Week in Tech/abc123XYZ", job.JobID)
	assert.Equal(t, "This Week in Tech", job.Feed)
	assert.Equal(t, "abc123XYZ", job.YTID)
	assert.Equal(t, srcPath, job.SrcPath)
	assert.Equal(t, "/srv/adstrip/adstrip-work/This Week in Tech/abc123XYZ.cut.mp4", job.DestPath)
	assert.Equal(t, feed.AggressivenessAggressive, job.Aggressiveness)
	assert.Equal(t, detectorModeLegacy, job.DetectorMode)
	assert.Equal(t, "2026-06-21T22:40:00Z", job.CreatedAt)
	// Attempt token is the enqueue time in ns - present and non-empty.
	assert.NotEmpty(t, job.Attempt)
}

func TestDivertDefaultsAggressivenessWhenEmpty(t *testing.T) {
	store := newFakeStore()
	b := newTestBarrier(store, time.Now(), defaultAdstripTimeout)

	cfg := &feed.Config{ID: "Feed"} // StripAds.Aggressiveness empty
	cfg.StripAds.Enabled = true
	require.NoError(t, b.divert(cfg, "yt1", "t", bytes.NewBufferString("x")))

	var job adstripJob
	require.NoError(t, json.Unmarshal(store.jobs["/srv/adstrip/adstrip-state/jobs/Feed/yt1.json"], &job))
	assert.Equal(t, feed.AggressivenessBalanced, job.Aggressiveness)
}

func TestDivertRejectsUnsafeIdentifier(t *testing.T) {
	store := newFakeStore()
	b := newTestBarrier(store, time.Now(), defaultAdstripTimeout)

	cfg := &feed.Config{ID: "Feed"}
	cfg.StripAds.Enabled = true

	err := b.divert(cfg, "../../escape", "t", bytes.NewBufferString("x"))
	assert.Error(t, err)
	assert.Empty(t, store.diverted, "nothing should be staged for an unsafe id")
	assert.Empty(t, store.jobs, "no job should be written for an unsafe id")
}

// testAttempt is the attempt token the table tests expect on a matching result.
const testAttempt = "1718000000000000000"

func mustResult(t *testing.T, state, outputPath, heldReason string) []byte {
	t.Helper()
	res := adstripResult{Schema: 1, State: state, OutputPath: outputPath, HeldReason: heldReason, Attempt: testAttempt}
	b, err := json.Marshal(&res)
	require.NoError(t, err)
	return b
}

func TestReconcileTable(t *testing.T) {
	now := time.Date(2026, 6, 21, 23, 0, 0, 0, time.UTC)
	created := now.Add(-10 * time.Minute) // well within timeout
	resultPath := "/srv/adstrip/adstrip-state/results/Feed/yt1.json"

	cases := []struct {
		name        string
		result      []byte // nil => no result file
		createdAt   time.Time
		timeout     time.Duration
		wantOutcome reconcileOutcome
		wantOutput  string
		wantReason  string
	}{
		{
			name:        "done_cut publishes",
			result:      mustResult(t, "done_cut", "/srv/adstrip/adstrip-work/Feed/yt1.cut.mp4", ""),
			createdAt:   created,
			timeout:     defaultAdstripTimeout,
			wantOutcome: reconcilePublish,
			wantOutput:  "/srv/adstrip/adstrip-work/Feed/yt1.cut.mp4",
		},
		{
			name:        "done_no_cuts publishes",
			result:      mustResult(t, "done_no_cuts", "/srv/adstrip/adstrip-work/Feed/yt1.cut.mp4", ""),
			createdAt:   created,
			timeout:     defaultAdstripTimeout,
			wantOutcome: reconcilePublish,
			wantOutput:  "/srv/adstrip/adstrip-work/Feed/yt1.cut.mp4",
		},
		{
			name:        "done_ missing output holds",
			result:      mustResult(t, "done_cut", "", ""),
			createdAt:   created,
			timeout:     defaultAdstripTimeout,
			wantOutcome: reconcileHold,
			wantReason:  "result output path does not match expected cut file",
		},
		{
			// Cardinal rule: a result pointing outputPath at the RAW mp4 must never
			// publish. The path-equality guard holds it.
			name:        "done_cut pointing at raw mp4 holds (never publish uncut)",
			result:      mustResult(t, "done_cut", "/srv/adstrip/adstrip-work/Feed/yt1.mp4", ""),
			createdAt:   created,
			timeout:     defaultAdstripTimeout,
			wantOutcome: reconcileHold,
			wantReason:  "result output path does not match expected cut file",
		},
		{
			// A bogus done_* state the contract does not define must not publish.
			name:        "unknown done_ state does not publish",
			result:      mustResult(t, "done_raw", "/srv/adstrip/adstrip-work/Feed/yt1.cut.mp4", ""),
			createdAt:   created,
			timeout:     defaultAdstripTimeout,
			wantOutcome: reconcilePending,
		},
		{
			// A result with the wrong schema is ignored (treated as not-ready).
			name:        "wrong schema result is not published",
			result:      []byte(`{"schema":99,"state":"done_cut","outputPath":"/srv/adstrip/adstrip-work/Feed/yt1.cut.mp4"}`),
			createdAt:   created,
			timeout:     defaultAdstripTimeout,
			wantOutcome: reconcilePending,
		},
		{
			name:        "failed_llm_unavailable holds with reason",
			result:      mustResult(t, "failed_llm_unavailable", "", "LM Studio down"),
			createdAt:   created,
			timeout:     defaultAdstripTimeout,
			wantOutcome: reconcileHold,
			wantReason:  "LM Studio down",
		},
		{
			name:        "failed_partial_detection holds (not treated as clean)",
			result:      mustResult(t, "failed_partial_detection", "", ""),
			createdAt:   created,
			timeout:     defaultAdstripTimeout,
			wantOutcome: reconcileHold,
			wantReason:  "failed_partial_detection",
		},
		{
			name:        "no result, within timeout, stays processing",
			result:      nil,
			createdAt:   created,
			timeout:     defaultAdstripTimeout,
			wantOutcome: reconcilePending,
		},
		{
			name:        "no result, past timeout, holds",
			result:      nil,
			createdAt:   now.Add(-7 * time.Hour),
			timeout:     defaultAdstripTimeout,
			wantOutcome: reconcileHold,
			wantReason:  "worker timeout",
		},
		{
			name:        "unknown state within timeout stays pending",
			result:      mustResult(t, "weird_state", "", ""),
			createdAt:   created,
			timeout:     defaultAdstripTimeout,
			wantOutcome: reconcilePending,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			if tc.result != nil {
				store.results[resultPath] = tc.result
			}
			b := newTestBarrier(store, now, tc.timeout)

			dec, err := b.reconcile("Feed", "yt1", tc.createdAt, testAttempt)
			require.NoError(t, err)
			assert.Equal(t, tc.wantOutcome, dec.outcome)
			if tc.wantOutput != "" {
				assert.Equal(t, tc.wantOutput, dec.outputPath)
			}
			if tc.wantReason != "" {
				assert.Equal(t, tc.wantReason, dec.heldReason)
			}
		})
	}
}

// TestDivertClearsStaleResult proves a fresh job for the same feed/ytid removes a
// leftover result first, so reconcile cannot publish stale output or hold a fresh
// attempt on an old failure.
func TestDivertClearsStaleResult(t *testing.T) {
	store := newFakeStore()
	resultPath := "/srv/adstrip/adstrip-state/results/Feed/yt1.json"
	store.results[resultPath] = mustResult(t, "failed_convert", "", "old failure")

	b := newTestBarrier(store, time.Now(), defaultAdstripTimeout)
	cfg := &feed.Config{ID: "Feed"}
	cfg.StripAds.Enabled = true

	require.NoError(t, b.divert(cfg, "yt1", "t", bytes.NewBufferString("raw")))
	assert.NotContains(t, store.results, resultPath, "stale result must be cleared on re-enqueue")
}

// TestTimedOutHoldsWhenJobMetadataMissing proves a processing episode with no
// trustworthy start time still times out (it is held), never stranded forever.
func TestTimedOutHoldsWhenJobMetadataMissing(t *testing.T) {
	b := newTestBarrier(newFakeStore(), time.Now(), defaultAdstripTimeout)
	dec, err := b.reconcile("Feed", "yt1", time.Time{}, "") // zero createdAt, no result file
	require.NoError(t, err)
	assert.Equal(t, reconcileHold, dec.outcome)
	assert.Equal(t, "worker timeout", dec.heldReason)
}

// TestReconcileIgnoresStaleAttempt proves a result written by a prior (crashed)
// attempt is not consumed by the current attempt: its attempt token differs, so it
// is ignored and we keep waiting rather than holding on someone else's failure.
func TestReconcileIgnoresStaleAttempt(t *testing.T) {
	now := time.Date(2026, 6, 21, 23, 0, 0, 0, time.UTC)
	store := newFakeStore()
	// A stale failed result from a previous attempt (different attempt token).
	stale := adstripResult{Schema: 1, State: "failed_convert", HeldReason: "old", Attempt: "999"}
	raw, _ := json.Marshal(&stale)
	store.results["/srv/adstrip/adstrip-state/results/Feed/yt1.json"] = raw

	b := newTestBarrier(store, now, defaultAdstripTimeout)
	dec, err := b.reconcile("Feed", "yt1", now.Add(-5*time.Minute), testAttempt)
	require.NoError(t, err)
	assert.Equal(t, reconcilePending, dec.outcome, "a stale-attempt result must not decide the current attempt")
}

// TestReconcileWithoutExpectedAttemptDoesNotConsumeResult proves that when the job
// metadata is gone (empty expectedAttempt) we trust NO result - even a well-formed
// done_cut whose output path matches - and hold once timed out, rather than letting
// an unattributable result publish or hold.
func TestReconcileWithoutExpectedAttemptDoesNotConsumeResult(t *testing.T) {
	now := time.Date(2026, 6, 21, 23, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.results["/srv/adstrip/adstrip-state/results/Feed/yt1.json"] =
		mustResult(t, "done_cut", "/srv/adstrip/adstrip-work/Feed/yt1.cut.mp4", "")

	b := newTestBarrier(store, now, defaultAdstripTimeout)

	// Within timeout: a valid result must NOT be consumed without an expected attempt.
	dec, err := b.reconcile("Feed", "yt1", now.Add(-1*time.Minute), "")
	require.NoError(t, err)
	assert.Equal(t, reconcilePending, dec.outcome)

	// Past timeout with no attributable result: hold, never publish.
	dec, err = b.reconcile("Feed", "yt1", now.Add(-7*time.Hour), "")
	require.NoError(t, err)
	assert.Equal(t, reconcileHold, dec.outcome)
}

// TestStripStatusesExcludedFromXML proves the two new statuses (and the existing
// non-Downloaded ones) never enter the RSS feed, while Downloaded does. This is the
// cardinal-rule guard: an episode being processed or held must not be served.
func TestStripStatusesExcludedFromXML(t *testing.T) {
	now := time.Now().UTC()
	mk := func(id string, status model.EpisodeStatus) *model.Episode {
		return &model.Episode{
			ID:       id,
			Title:    id,
			Status:   status,
			PubDate:  now,
			Size:     1,
			VideoURL: "https://example.com/" + id,
		}
	}

	f := &model.Feed{
		Title:       "Feed",
		Description: "desc",
		Format:      model.FormatVideo,
		PubDate:     now,
		Episodes: []*model.Episode{
			mk("served", model.EpisodeDownloaded),
			mk("processing", model.EpisodeStripProcessing),
			mk("held", model.EpisodeStripFailed),
			mk("new", model.EpisodeNew),
		},
	}

	cfg := &feed.Config{ID: "feed"}
	podcast, err := feed.Build(context.Background(), f, cfg, "http://localhost")
	require.NoError(t, err)

	xml := podcast.String()
	assert.Contains(t, xml, "served", "downloaded episode must appear in RSS")
	assert.NotContains(t, xml, "processing", "strip_processing must be excluded from RSS")
	assert.NotContains(t, xml, "held", "strip_failed must be excluded from RSS")
	assert.NotContains(t, xml, ">new<", "new episode must be excluded from RSS")

	// Sanity: exactly one item in the feed.
	assert.Equal(t, 1, strings.Count(xml, "<item>"))
	_ = itunes.Item{} // keep the itunes import meaningful if Build internals change
}
