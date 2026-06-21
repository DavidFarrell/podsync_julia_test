// clipclean: one-off maintenance tool. Marks already-downloaded short episodes
// as "cleaned" (so they drop out of the served RSS via pkg/feed/xml.go:112 and are
// never re-downloaded via services/update/updater.go:163) for a set of feeds, each
// with its own duration floor. DB-only: it does NOT touch files on disk - trashing
// the mp4s is done separately by the operator. Uses podsync's own db package so key
// encoding + JSON serialization are guaranteed correct.
//
// REQUIRES podsync to be stopped (badger holds an exclusive lock).
//
//   go run ./cmd/clipclean -db /path/to/db            # dry-run (default), prints what WOULD change
//   go run ./cmd/clipclean -db /path/to/db -apply     # writes status=cleaned
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/mxpv/podsync/pkg/db"
	"github.com/mxpv/podsync/pkg/model"
)

// Per-feed duration floor in seconds. A downloaded episode strictly shorter than
// its feed's floor gets marked cleaned. Keys MUST match the feed ID exactly.
var cuts = map[string]int64{
	"anthropic":              240, // David: keep anything over 4 min
	"openai":                 240, // David: keep anything over 4 min
	"Nerd Snipe":             600,
	"Pragmatic Engineer":     600,
	"AI Stories Neil Leiser": 600,
	"Being Ian with Jordan":  600,
	"Hamel":                  180,
	"Stavvy":                 600,
	"feed35":                 300,
	"ThursdAI Video":         600,
}

func main() {
	dbDir := flag.String("db", "", "path to podsync badger db directory")
	apply := flag.Bool("apply", false, "actually write status=cleaned (default: dry-run)")
	flag.Parse()
	if *dbDir == "" {
		fmt.Fprintln(os.Stderr, "ERROR: -db is required")
		os.Exit(2)
	}

	storage, err := db.NewBadger(&db.Config{Dir: *dbDir})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR open db: %v\n", err)
		os.Exit(1)
	}
	defer storage.Close()

	ctx := context.Background()

	// First, enumerate every feed ID present, so the operator can verify the
	// cut-map keys line up with reality.
	var allFeeds []string
	if err := storage.WalkFeeds(ctx, func(f *model.Feed) error {
		allFeeds = append(allFeeds, f.ID)
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR walk feeds: %v\n", err)
		os.Exit(1)
	}
	_ = allFeeds // stored Feed blobs don't serialize ID; episodes are keyed by the config feed ID directly.

	type hit struct {
		feed, id string
		dur      int64
	}
	var hits []hit
	grandKept := map[string]int{}
	grandDownloaded := map[string]int{}

	// Iterate the cut-map directly: episodes live under episode/<feedID>/<id>
	// where feedID is the TOML config key (cmd/podsync/config.go:68 f.ID = id).
	feedNames := make([]string, 0, len(cuts))
	for name := range cuts {
		feedNames = append(feedNames, name)
	}
	sort.Strings(feedNames)
	for _, id := range feedNames {
		floor := cuts[id]
		err := storage.WalkEpisodes(ctx, id, func(e *model.Episode) error {
			if e.Status != model.EpisodeDownloaded {
				return nil
			}
			grandDownloaded[id]++
			if e.Duration < floor {
				hits = append(hits, hit{feed: id, id: e.ID, dur: e.Duration})
			} else {
				grandKept[id]++
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR walk episodes %q: %v\n", id, err)
			os.Exit(1)
		}
	}

	// Group + print.
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].feed != hits[j].feed {
			return hits[i].feed < hits[j].feed
		}
		return hits[i].dur < hits[j].dur
	})
	perFeed := map[string]int{}
	for _, h := range hits {
		perFeed[h.feed]++
	}
	for _, id := range feedNames {
		fmt.Printf("[%s] floor %ds: %d downloaded -> %d to clean, %d kept\n", id, cuts[id], grandDownloaded[id], perFeed[id], grandKept[id])
	}
	fmt.Printf("\nTOTAL to clean: %d episodes\n\n", len(hits))

	if !*apply {
		fmt.Println("DRY-RUN - no changes written. Episodes that WOULD be cleaned:")
		for _, h := range hits {
			fmt.Printf("WOULD-CLEAN\t%s\t%s\t%ds\n", h.feed, h.id, h.dur)
		}
		return
	}

	// Apply.
	var done, failed int
	for _, h := range hits {
		err := storage.UpdateEpisode(h.feed, h.id, func(e *model.Episode) error {
			e.Status = model.EpisodeCleaned
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL\t%s\t%s\t%v\n", h.feed, h.id, err)
			failed++
			continue
		}
		// This line is consumed by the operator to trash the matching media file.
		fmt.Printf("CLEANED\t%s\t%s\t%ds\n", h.feed, h.id, h.dur)
		done++
	}
	fmt.Printf("\nDONE: %d cleaned, %d failed\n", done, failed)
	if failed > 0 {
		os.Exit(1)
	}
}
