package update

// Ad-strip publish barrier.
//
// Cardinal rule of the whole feature: never serve an UNCUT file. For a feed with
// strip_ads enabled we do NOT copy the raw download into the served tree. Instead
// we divert the raw mp4 into an UNSERVED host staging dir, write a job JSON for the
// host worker, and mark the episode EpisodeStripProcessing (which xml.go excludes
// from RSS). A later update cycle reconciles: on a done_* result we copy the CUT
// file into the served tree and mark EpisodeDownloaded; on a failed_* result (or a
// timeout) we mark EpisodeStripFailed and HOLD the episode - we never fall back to
// the raw file.
//
// Publishing is decoupled from cutting (the worker is serial and a long episode can
// take tens of minutes). The download goroutine never blocks waiting for a cut: it
// enqueues and returns, and reconciliation happens at the start of subsequent
// cycles. A crash between enqueue and reconcile loses nothing - the episode stays
// EpisodeStripProcessing and is retried on the next cycle.
//
// All path components coming from feed/episode identifiers are treated as untrusted
// and sanitised; a component that cannot be made safe fails the divert (so the
// episode is marked errored and retried) rather than being joined into a path.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/mxpv/podsync/pkg/feed"
)

// jobSchema is the contract version shared with the host daemon (see
// design/adstrip-job-result-contract.md). Bump only with the daemon.
const jobSchema = 1

// detectorMode is locked to legacy for the MVP - the UI never exposes a picker.
const detectorModeLegacy = "legacy"

// defaultAdstripTimeout is how long an episode may sit in EpisodeStripProcessing
// before reconciliation gives up and HOLDS it. The worker is serial, so a backlog
// is normal; this must comfortably exceed transcribe + re-encode of the longest
// episode plus queue depth. Generous on purpose.
const defaultAdstripTimeout = 6 * time.Hour

// adstripClock is injectable so reconciliation timeouts are testable without sleeping.
type adstripClock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// adstripStore is the disk surface the barrier touches. The real implementation
// writes the host staging/state tree; tests inject a fake to stay hermetic.
type adstripStore interface {
	// DivertRaw moves the freshly downloaded temp file into the unserved staging
	// dir at srcPath. It must NOT write into the served tree.
	DivertRaw(srcReader io.Reader, srcPath string) error
	// WriteJob atomically writes the job JSON for the daemon.
	WriteJob(jobPath string, job []byte) error
	// ReadResult returns the result JSON for an episode, or os.ErrNotExist if the
	// daemon has not finished yet. Readers must tolerate the missing file.
	ReadResult(resultPath string) ([]byte, error)
	// OpenOutput opens the worker's cut output for copying into the served tree.
	OpenOutput(outputPath string) (io.ReadCloser, error)
	// RemoveResult deletes any stale result from a previous run before a new job is
	// enqueued, so reconcile cannot consume a leftover result for a fresh attempt.
	// A missing file is not an error.
	RemoveResult(resultPath string) error
}

// adstripJob mirrors the Job JSON in the contract doc. Field order/names are part
// of the contract - do not rename without updating the daemon.
type adstripJob struct {
	Schema         int    `json:"schema"`
	JobID          string `json:"jobId"`
	Feed           string `json:"feed"`
	YTID           string `json:"ytid"`
	SrcPath        string `json:"srcPath"`
	DestPath       string `json:"destPath"`
	Aggressiveness string `json:"aggressiveness"`
	DetectorMode   string `json:"detectorMode"`
	Title          string `json:"title"`
	CreatedAt      string `json:"createdAt"`
	// Attempt uniquely identifies this enqueue. The daemon MUST echo it verbatim
	// into the result. Reconcile only accepts a result whose attempt matches the
	// current job, so a result left over from a crashed-then-retried prior attempt
	// is never consumed as if it belonged to the new one.
	Attempt string `json:"attempt"`
}

// adstripResult mirrors the Result JSON. Only the fields the barrier acts on are
// modelled; unknown fields are ignored (the UI reads the rest).
type adstripResult struct {
	Schema     int    `json:"schema"`
	State      string `json:"state"`
	OutputPath string `json:"outputPath"`
	HeldReason string `json:"heldReason"`
	// Attempt is the job's attempt token echoed back by the daemon. A result whose
	// attempt does not match the current job is stale and is ignored.
	Attempt string `json:"attempt"`
}

// adstripConfig holds the resolved root + timeout for the barrier.
type adstripConfig struct {
	// Root is the host directory that holds adstrip-work/ and adstrip-state/.
	// Bind-mounted into the container at the same path.
	Root string
	// Timeout bounds how long an episode may stay processing before being held.
	Timeout time.Duration
}

// adstripBarrier carries out divert and reconcile against an injected store/clock.
type adstripBarrier struct {
	cfg   adstripConfig
	store adstripStore
	clock adstripClock
}

// adstripRootEnv lets ops point the staging/state tree at the bind-mounted path.
const adstripRootEnv = "ADSTRIP_ROOT"

// newAdstripBarrier builds the barrier from the environment. Root comes from
// ADSTRIP_ROOT (default ./adstrip relative to the working dir, which the container
// sets via the bind mount). Returns nil if the env is unset AND the default cannot
// be used - callers treat a nil barrier as "strip-ads not configured".
func newAdstripBarrier() *adstripBarrier {
	root := os.Getenv(adstripRootEnv)
	if root == "" {
		root = "/app/adstrip"
	}
	return &adstripBarrier{
		cfg:   adstripConfig{Root: root, Timeout: defaultAdstripTimeout},
		store: &localAdstripStore{},
		clock: realClock{},
	}
}

// localAdstripStore is the production store: real disk, atomic writes.
type localAdstripStore struct{}

func (localAdstripStore) DivertRaw(srcReader io.Reader, srcPath string) error {
	if err := os.MkdirAll(filepath.Dir(srcPath), 0o755); err != nil {
		return errors.Wrapf(err, "failed to mkdir staging dir for %s", srcPath)
	}
	// Write to a temp sibling then rename, so a crash mid-copy never leaves a
	// truncated raw that the daemon could pick up.
	tmp := srcPath + ".tmp"
	dest, err := os.Create(tmp)
	if err != nil {
		return errors.Wrapf(err, "failed to create staging temp %s", tmp)
	}
	if _, err := io.Copy(dest, srcReader); err != nil {
		dest.Close()
		os.Remove(tmp)
		return errors.Wrap(err, "failed to copy raw into staging")
	}
	if err := dest.Close(); err != nil {
		os.Remove(tmp)
		return errors.Wrap(err, "failed to close staging temp")
	}
	if err := os.Rename(tmp, srcPath); err != nil {
		os.Remove(tmp)
		return errors.Wrap(err, "failed to rename staging temp into place")
	}
	return nil
}

func (localAdstripStore) WriteJob(jobPath string, job []byte) error {
	if err := os.MkdirAll(filepath.Dir(jobPath), 0o755); err != nil {
		return errors.Wrapf(err, "failed to mkdir jobs dir for %s", jobPath)
	}
	tmp := jobPath + ".tmp"
	if err := os.WriteFile(tmp, job, 0o644); err != nil {
		return errors.Wrapf(err, "failed to write job temp %s", tmp)
	}
	if err := os.Rename(tmp, jobPath); err != nil {
		os.Remove(tmp)
		return errors.Wrap(err, "failed to rename job temp into place")
	}
	return nil
}

func (localAdstripStore) ReadResult(resultPath string) ([]byte, error) {
	return os.ReadFile(resultPath)
}

func (localAdstripStore) OpenOutput(outputPath string) (io.ReadCloser, error) {
	return os.Open(outputPath)
}

func (localAdstripStore) RemoveResult(resultPath string) error {
	if err := os.Remove(resultPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// safePathComponent validates a single untrusted path component. It rejects empty,
// ".", "..", a leading dot, and anything containing a path separator or NUL. We do
// not rewrite - a name we cannot trust fails the divert so the episode is retried,
// which is safer than guessing a sanitised name and silently mismatching the daemon.
func safePathComponent(name string) (string, error) {
	if name == "" {
		return "", errors.New("empty path component")
	}
	if name == "." || name == ".." {
		return "", errors.Errorf("disallowed path component %q", name)
	}
	if strings.HasPrefix(name, ".") {
		return "", errors.Errorf("leading-dot path component %q", name)
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, os.PathSeparator) {
		return "", errors.Errorf("path separator in component %q", name)
	}
	if strings.ContainsRune(name, 0) {
		return "", errors.Errorf("NUL in path component %q", name)
	}
	return name, nil
}

// paths bundles the four contract paths for one episode.
type adstripPaths struct {
	src    string // adstrip-work/<feed>/<ytid>.mp4
	cut    string // adstrip-work/<feed>/<ytid>.cut.mp4
	job    string // adstrip-state/jobs/<feed>/<ytid>.json
	result string // adstrip-state/results/<feed>/<ytid>.json
}

// resolvePaths sanitises feed + ytid and builds the four contract paths. Returns an
// error if either component is unsafe.
func (b *adstripBarrier) resolvePaths(feedName, ytid string) (adstripPaths, error) {
	safeFeed, err := safePathComponent(feedName)
	if err != nil {
		return adstripPaths{}, errors.Wrap(err, "unsafe feed name")
	}
	safeYTID, err := safePathComponent(ytid)
	if err != nil {
		return adstripPaths{}, errors.Wrap(err, "unsafe ytid")
	}
	work := filepath.Join(b.cfg.Root, "adstrip-work", safeFeed)
	jobs := filepath.Join(b.cfg.Root, "adstrip-state", "jobs", safeFeed)
	results := filepath.Join(b.cfg.Root, "adstrip-state", "results", safeFeed)
	return adstripPaths{
		src:    filepath.Join(work, safeYTID+".mp4"),
		cut:    filepath.Join(work, safeYTID+".cut.mp4"),
		job:    filepath.Join(jobs, safeYTID+".json"),
		result: filepath.Join(results, safeYTID+".json"),
	}, nil
}

// divert moves the raw temp file into staging and writes the job JSON. On success
// the caller marks the episode EpisodeStripProcessing. The job carries the source
// reader (the still-open downloaded temp file).
func (b *adstripBarrier) divert(feedCfg *feed.Config, ytid, title string, raw io.Reader) error {
	paths, err := b.resolvePaths(feedCfg.ID, ytid)
	if err != nil {
		return err
	}

	// Clear any stale result from a previous attempt at this same feed/ytid first, so
	// reconcile cannot consume a leftover done_*/failed_* for this fresh job.
	if err := b.store.RemoveResult(paths.result); err != nil {
		return errors.Wrap(err, "failed to clear stale result")
	}

	// Stage the raw file next. Only once it is fully in place do we write the job, so
	// the daemon can never observe a job pointing at a missing/partial source.
	if err := b.store.DivertRaw(raw, paths.src); err != nil {
		return errors.Wrap(err, "failed to divert raw into staging")
	}

	// Normalize aggressiveness at the boundary that writes the daemon contract, so an
	// unknown value can never reach the worker even if a caller bypassed config load.
	aggr := feedCfg.StripAds
	aggr.Normalize()

	// Attempt token: nanosecond timestamp of this enqueue. Distinct per enqueue, so a
	// retried job will not accept a previous attempt's stray result.
	now := b.clock.Now().UTC()
	job := adstripJob{
		Schema:         jobSchema,
		JobID:          feedCfg.ID + "/" + ytid,
		Feed:           feedCfg.ID,
		YTID:           ytid,
		SrcPath:        paths.src,
		DestPath:       paths.cut,
		Aggressiveness: aggr.Aggressiveness,
		DetectorMode:   detectorModeLegacy,
		Title:          title,
		CreatedAt:      now.Format(time.RFC3339),
		Attempt:        strconv.FormatInt(now.UnixNano(), 10),
	}
	encoded, err := json.MarshalIndent(&job, "", "  ")
	if err != nil {
		return errors.Wrap(err, "failed to encode job json")
	}
	if err := b.store.WriteJob(paths.job, encoded); err != nil {
		return errors.Wrap(err, "failed to write job json")
	}
	return nil
}

// reconcileOutcome is what reconcile decided for one processing episode.
type reconcileOutcome int

const (
	// reconcilePending: no result yet and not timed out - leave it processing.
	reconcilePending reconcileOutcome = iota
	// reconcilePublish: a done_* result - copy the cut output and mark Downloaded.
	reconcilePublish
	// reconcileHold: a failed_* result or a timeout - mark StripFailed (HELD).
	reconcileHold
)

// reconcileDecision carries the outcome plus the data the caller needs to act.
type reconcileDecision struct {
	outcome    reconcileOutcome
	outputPath string // set when outcome == reconcilePublish
	heldReason string // set when outcome == reconcileHold
}

// timedOut reports whether a processing episode has exceeded its timeout. A zero
// createdAt (job metadata missing) still times out, measured from a fallback caller-
// supplied baseline, so a lost job can never strand an episode in processing forever.
func (b *adstripBarrier) timedOut(createdAt time.Time) bool {
	if createdAt.IsZero() {
		return true // no trustworthy start time - do not keep it processing
	}
	return b.clock.Now().Sub(createdAt) > b.cfg.Timeout
}

// reconcile inspects the result file (if any) for a processing episode and decides
// what to do. createdAt is when the episode entered processing (the job timestamp);
// it bounds the timeout. This is pure aside from the result read, so it is easy to
// table-test.
//
// Cardinal-rule guards live here: only the two explicit success states publish, and
// only when the result's outputPath is EXACTLY the resolved .cut.mp4 for this episode
// - a daemon bug or hostile result that points outputPath at the raw .mp4 (or
// anywhere else) is held, never published. Everything that is not an unambiguous,
// validated success holds rather than serves an uncut file.
func (b *adstripBarrier) reconcile(feedName, ytid string, createdAt time.Time, expectedAttempt string) (reconcileDecision, error) {
	paths, err := b.resolvePaths(feedName, ytid)
	if err != nil {
		// An unsafe identifier on a processing episode should not happen (we wrote
		// it ourselves), but if it does, hold rather than loop forever.
		return reconcileDecision{outcome: reconcileHold, heldReason: "unsafe identifier"}, nil
	}

	raw, err := b.store.ReadResult(paths.result)
	if err != nil {
		if os.IsNotExist(err) {
			// No result yet. Hold once the timeout passes, else keep waiting.
			if b.timedOut(createdAt) {
				return reconcileDecision{outcome: reconcileHold, heldReason: "worker timeout"}, nil
			}
			return reconcileDecision{outcome: reconcilePending}, nil
		}
		return reconcileDecision{}, errors.Wrap(err, "failed to read result json")
	}

	var res adstripResult
	if err := json.Unmarshal(raw, &res); err != nil || res.Schema != jobSchema {
		// A corrupt/partial result, or a schema we do not understand, is treated as
		// not-yet-ready (the daemon writes atomically, so a partial read is
		// transient); the timeout eventually holds it.
		if b.timedOut(createdAt) {
			return reconcileDecision{outcome: reconcileHold, heldReason: "worker timeout"}, nil
		}
		return reconcileDecision{outcome: reconcilePending}, nil
	}

	// Only act on a result we can attribute to THIS attempt. If the job metadata is
	// missing (empty expectedAttempt) we cannot validate ownership, so we trust no
	// result. If the result's attempt token differs, it belongs to a prior crashed-
	// then-retried attempt. Either way: do not consume it - wait, then hold on
	// timeout. This closes the stale-result race, including the case where the job
	// file did not survive to reconcile time.
	if expectedAttempt == "" || res.Attempt != expectedAttempt {
		if b.timedOut(createdAt) {
			return reconcileDecision{outcome: reconcileHold, heldReason: "worker timeout"}, nil
		}
		return reconcileDecision{outcome: reconcilePending}, nil
	}

	switch res.State {
	case "done_cut", "done_no_cuts":
		// Require the output to be EXACTLY this episode's cut file. This is the guard
		// that stops a result from ever publishing the raw mp4 or any other path.
		if res.OutputPath != paths.cut {
			return reconcileDecision{outcome: reconcileHold, heldReason: "result output path does not match expected cut file"}, nil
		}
		return reconcileDecision{outcome: reconcilePublish, outputPath: paths.cut}, nil
	case "failed_llm_unavailable", "failed_transcribe", "failed_partial_detection",
		"failed_convert", "failed_bad_input":
		reason := res.HeldReason
		if reason == "" {
			reason = res.State
		}
		return reconcileDecision{outcome: reconcileHold, heldReason: reason}, nil
	default:
		// Any other state (including a bogus done_*/failed_* the contract does not
		// define) is not a publish signal. Hold once timed out; otherwise wait for a
		// corrected result.
		if b.timedOut(createdAt) {
			return reconcileDecision{outcome: reconcileHold, heldReason: fmt.Sprintf("unknown state %q after timeout", res.State)}, nil
		}
		return reconcileDecision{outcome: reconcilePending}, nil
	}
}

// loadJobMeta reads the job JSON to recover when the episode entered processing and
// which attempt is current, so reconcile can apply its timeout and reject a stale
// (other-attempt) result. A missing/unreadable job yields a zero time and empty
// attempt; timedOut(zero) holds, so a lost job is never stranded.
func (b *adstripBarrier) loadJobMeta(feedName, ytid string) (createdAt time.Time, attempt string) {
	paths, err := b.resolvePaths(feedName, ytid)
	if err != nil {
		return time.Time{}, ""
	}
	raw, err := os.ReadFile(paths.job)
	if err != nil {
		return time.Time{}, ""
	}
	var job adstripJob
	if err := json.Unmarshal(raw, &job); err != nil {
		return time.Time{}, ""
	}
	t, err := time.Parse(time.RFC3339, job.CreatedAt)
	if err != nil {
		t = time.Time{}
	}
	return t, job.Attempt
}
