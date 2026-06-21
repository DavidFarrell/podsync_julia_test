package update

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/mxpv/podsync/pkg/builder"
	"github.com/mxpv/podsync/pkg/db"
	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/fs"
	"github.com/mxpv/podsync/pkg/model"
	"github.com/mxpv/podsync/pkg/ytdl"
)

type Downloader interface {
	Download(ctx context.Context, feedConfig *feed.Config, episode *model.Episode) (io.ReadCloser, error)
}

type TokenList []string

type Manager struct {
	hostname   string
	downloader Downloader
	db         db.Storage
	fs         fs.Storage
	feeds      map[string]*feed.Config
	keys       map[model.Provider]feed.KeyProvider
	adstrip    *adstripBarrier
}

func NewUpdater(
	feeds map[string]*feed.Config,
	keys map[model.Provider]feed.KeyProvider,
	hostname string,
	downloader Downloader,
	db db.Storage,
	fs fs.Storage,
) (*Manager, error) {
	return &Manager{
		hostname:   hostname,
		downloader: downloader,
		db:         db,
		fs:         fs,
		feeds:      feeds,
		keys:       keys,
		adstrip:    newAdstripBarrier(),
	}, nil
}

func (u *Manager) Update(ctx context.Context, feedConfig *feed.Config) error {
	log.WithFields(log.Fields{
		"feed_id": feedConfig.ID,
		"format":  feedConfig.Format,
		"quality": feedConfig.Quality,
	}).Infof("-> updating %s", feedConfig.URL)

	started := time.Now()

	if err := u.updateFeed(ctx, feedConfig); err != nil {
		return errors.Wrap(err, "update failed")
	}

	// Publish any ad-strip episodes whose cut results have landed since last cycle,
	// and hold any that have timed out. Runs before XML is rebuilt below so a
	// freshly-published episode appears in this cycle's feed. Errors here are
	// logged, not fatal - a reconcile failure must not stall the feed update.
	if err := u.reconcileStripAds(ctx, feedConfig); err != nil {
		log.WithError(err).Error("ad-strip reconcile failed")
	}

	if err := u.downloadEpisodes(ctx, feedConfig); err != nil {
		return errors.Wrap(err, "download failed")
	}

	if err := u.cleanup(ctx, feedConfig); err != nil {
		log.WithError(err).Error("cleanup failed")
	}

	if err := u.buildXML(ctx, feedConfig); err != nil {
		return errors.Wrap(err, "xml build failed")
	}

	if err := u.buildOPML(ctx); err != nil {
		return errors.Wrap(err, "opml build failed")
	}

	elapsed := time.Since(started)
	log.Infof("successfully updated feed in %s", elapsed)
	return nil
}

// updateFeed pulls API for new episodes and saves them to database
func (u *Manager) updateFeed(ctx context.Context, feedConfig *feed.Config) error {
	info, err := builder.ParseURL(feedConfig.URL)
	if err != nil {
		return errors.Wrapf(err, "failed to parse URL: %s", feedConfig.URL)
	}

	keyProvider, ok := u.keys[info.Provider]
	if !ok {
		return errors.Errorf("key provider %q not loaded", info.Provider)
	}

	// Create an updater for this feed type
	provider, err := builder.New(ctx, info.Provider, keyProvider.Get())
	if err != nil {
		return err
	}

	// Query API to get episodes
	log.Debug("building feed")
	result, err := provider.Build(ctx, feedConfig)
	if err != nil {
		return err
	}

	log.Debugf("received %d episode(s) for %q", len(result.Episodes), result.Title)

	episodeSet := make(map[string]struct{})
	if err := u.db.WalkEpisodes(ctx, feedConfig.ID, func(episode *model.Episode) error {
		if episode.Status != model.EpisodeDownloaded && episode.Status != model.EpisodeCleaned {
			episodeSet[episode.ID] = struct{}{}
		}
		return nil
	}); err != nil {
		return err
	}

	if err := u.db.AddFeed(ctx, feedConfig.ID, result); err != nil {
		return err
	}

	for _, episode := range result.Episodes {
		delete(episodeSet, episode.ID)
	}

	// removing episodes that are no longer available in the feed and not downloaded or cleaned
	for id := range episodeSet {
		log.Infof("removing episode %q", id)
		err := u.db.DeleteEpisode(feedConfig.ID, id)
		if err != nil {
			return err
		}
	}

	log.Debug("successfully saved updates to storage")
	return nil
}

func (u *Manager) downloadEpisodes(ctx context.Context, feedConfig *feed.Config) error {
	var (
		feedID       = feedConfig.ID
		downloadList []*model.Episode
		pageSize     = feedConfig.PageSize
	)

	log.WithField("page_size", pageSize).Info("downloading episodes")

	// Build the list of files to download
	if err := u.db.WalkEpisodes(ctx, feedID, func(episode *model.Episode) error {
		var (
			logger = log.WithFields(log.Fields{"episode_id": episode.ID})
		)
		if episode.Status != model.EpisodeNew && episode.Status != model.EpisodeError {
			// File already downloaded
			logger.Infof("skipping due to already downloaded")
			return nil
		}

		if !matchFilters(episode, &feedConfig.Filters) {
			return nil
		}

		// Limit the number of episodes downloaded at once
		pageSize--
		if pageSize < 0 {
			return nil
		}

		log.Debugf("adding %s (%q) to queue", episode.ID, episode.Title)
		downloadList = append(downloadList, episode)
		return nil
	}); err != nil {
		return errors.Wrapf(err, "failed to build update list")
	}

	var (
		downloadCount = len(downloadList)
		downloaded    = 0
	)

	if downloadCount > 0 {
		log.Infof("download count: %d", downloadCount)
	} else {
		log.Info("no episodes to download")
		return nil
	}

	// Download pending episodes

	for idx, episode := range downloadList {
		var (
			logger      = log.WithFields(log.Fields{"index": idx, "episode_id": episode.ID})
			episodeName = feed.EpisodeName(feedConfig, episode)
		)

		// Check whether episode already exists
		size, err := u.fs.Size(ctx, fmt.Sprintf("%s/%s", feedID, episodeName))
		if err == nil {
			logger.Infof("episode %q already exists on disk", episode.ID)

			// File already exists, update file status and disk size
			if err := u.db.UpdateEpisode(feedID, episode.ID, func(episode *model.Episode) error {
				episode.Size = size
				episode.Status = model.EpisodeDownloaded
				return nil
			}); err != nil {
				logger.WithError(err).Error("failed to update file info")
				return err
			}

			continue
		} else if os.IsNotExist(err) {
			// Will download, do nothing here
		} else {
			logger.WithError(err).Error("failed to stat file")
			return err
		}

		// Download episode to disk
		// We download the episode to a temp directory first to avoid downloading this file by clients
		// while still being processed by youtube-dl (e.g. a file is being downloaded from YT or encoding in progress)

		logger.Infof("! downloading episode %s", episode.VideoURL)
		tempFile, err := u.downloader.Download(ctx, feedConfig, episode)
		if err != nil {
			// YouTube might block host with HTTP Error 429: Too Many Requests
			// We still need to generate XML, so just stop sending download requests and
			// retry next time
			if err == ytdl.ErrTooManyRequests {
				logger.Warn("server responded with a 'Too Many Requests' error")
				break
			}

			if err := u.db.UpdateEpisode(feedID, episode.ID, func(episode *model.Episode) error {
				episode.Status = model.EpisodeError
				return nil
			}); err != nil {
				return err
			}

			continue
		}

		// Ad-strip feeds: divert the raw download into the unserved staging tree and
		// enqueue a cut job instead of publishing. The episode goes EpisodeStripProcessing
		// (excluded from RSS) and is published later by reconcileStripAds once the worker
		// has produced a validated cut. We never copy the raw into the served tree.
		if u.adstrip != nil && feedConfig.StripAds.Enabled {
			err := u.adstrip.divert(feedConfig, episode.ID, episode.Title, tempFile)
			tempFile.Close()
			if err != nil {
				// Could not stage / enqueue. Mark errored so it is retried next cycle;
				// crucially we did NOT publish the uncut file.
				logger.WithError(err).Error("failed to divert episode to ad-strip staging")
				if uerr := u.db.UpdateEpisode(feedID, episode.ID, func(episode *model.Episode) error {
					episode.Status = model.EpisodeError
					return nil
				}); uerr != nil {
					return uerr
				}
				continue
			}

			logger.Infof("diverted episode %q to ad-strip worker", episode.ID)
			if err := u.db.UpdateEpisode(feedID, episode.ID, func(episode *model.Episode) error {
				episode.Status = model.EpisodeStripProcessing
				return nil
			}); err != nil {
				return err
			}

			downloaded++
			continue
		}

		logger.Debug("copying file")
		fileSize, err := u.fs.Create(ctx, fmt.Sprintf("%s/%s", feedID, episodeName), tempFile)
		tempFile.Close()
		if err != nil {
			logger.WithError(err).Error("failed to copy file")
			return err
		}

		// Update file status in database

		logger.Infof("successfully downloaded file %q", episode.ID)
		if err := u.db.UpdateEpisode(feedID, episode.ID, func(episode *model.Episode) error {
			episode.Size = fileSize
			episode.Status = model.EpisodeDownloaded
			return nil
		}); err != nil {
			return err
		}

		downloaded++
	}

	log.Infof("downloaded %d episode(s)", downloaded)
	return nil
}

// reconcileStripAds publishes or holds every episode currently in
// EpisodeStripProcessing for this feed, based on the worker's result file. A
// missing result that has not timed out is left untouched (still processing). The
// uncut file is never served: a hold marks EpisodeStripFailed, and on publish we
// copy the CUT output, never the raw.
func (u *Manager) reconcileStripAds(ctx context.Context, feedConfig *feed.Config) error {
	if u.adstrip == nil || !feedConfig.StripAds.Enabled {
		return nil
	}

	var processing []*model.Episode
	if err := u.db.WalkEpisodes(ctx, feedConfig.ID, func(episode *model.Episode) error {
		if episode.Status == model.EpisodeStripProcessing {
			// Copy out of the walk - we mutate the DB below.
			e := *episode
			processing = append(processing, &e)
		}
		return nil
	}); err != nil {
		return errors.Wrap(err, "failed to walk episodes for reconcile")
	}

	for _, episode := range processing {
		logger := log.WithFields(log.Fields{"feed_id": feedConfig.ID, "episode_id": episode.ID})

		createdAt, attempt := u.adstrip.loadJobMeta(feedConfig.ID, episode.ID)
		decision, err := u.adstrip.reconcile(feedConfig.ID, episode.ID, createdAt, attempt)
		if err != nil {
			logger.WithError(err).Error("failed to reconcile ad-strip episode")
			continue
		}

		switch decision.outcome {
		case reconcilePending:
			// Worker still busy (or backlog). Leave it processing.
			continue

		case reconcileHold:
			logger.Warnf("holding ad-strip episode: %s", decision.heldReason)
			if err := u.db.UpdateEpisode(feedConfig.ID, episode.ID, func(e *model.Episode) error {
				e.Status = model.EpisodeStripFailed
				return nil
			}); err != nil {
				logger.WithError(err).Error("failed to mark episode held")
			}

		case reconcilePublish:
			cut, err := u.adstrip.store.OpenOutput(decision.outputPath)
			if err != nil {
				// The result said done but the cut is unreadable. Do NOT publish the
				// raw. Retry on a later cycle, but HOLD once the timeout passes so a
				// permanently-broken cut cannot strand the episode in processing.
				if u.adstrip.timedOut(createdAt) {
					logger.WithError(err).Warn("cut output unreadable past timeout; holding")
					if uerr := u.db.UpdateEpisode(feedConfig.ID, episode.ID, func(e *model.Episode) error {
						e.Status = model.EpisodeStripFailed
						return nil
					}); uerr != nil {
						logger.WithError(uerr).Error("failed to mark episode held")
					}
				} else {
					logger.WithError(err).Error("failed to open cut output; leaving processing")
				}
				continue
			}

			episodeName := feed.EpisodeName(feedConfig, episode)
			size, copyErr := u.fs.Create(ctx, fmt.Sprintf("%s/%s", feedConfig.ID, episodeName), cut)
			cut.Close()
			if copyErr != nil {
				// Same reasoning as the open failure: retry, but hold after timeout.
				if u.adstrip.timedOut(createdAt) {
					logger.WithError(copyErr).Warn("cut copy failed past timeout; holding")
					if uerr := u.db.UpdateEpisode(feedConfig.ID, episode.ID, func(e *model.Episode) error {
						e.Status = model.EpisodeStripFailed
						return nil
					}); uerr != nil {
						logger.WithError(uerr).Error("failed to mark episode held")
					}
				} else {
					logger.WithError(copyErr).Error("failed to copy cut into served tree; leaving processing")
				}
				continue
			}

			logger.Infof("published ad-stripped episode %q", episode.ID)
			if err := u.db.UpdateEpisode(feedConfig.ID, episode.ID, func(e *model.Episode) error {
				e.Size = size
				e.Status = model.EpisodeDownloaded
				return nil
			}); err != nil {
				logger.WithError(err).Error("failed to mark episode downloaded")
			}
		}
	}

	return nil
}

func (u *Manager) buildXML(ctx context.Context, feedConfig *feed.Config) error {
	f, err := u.db.GetFeed(ctx, feedConfig.ID)
	if err != nil {
		return err
	}

	// Build iTunes XML feed with data received from builder
	log.Debug("building iTunes podcast feed")
	podcast, err := feed.Build(ctx, f, feedConfig, u.hostname)
	if err != nil {
		return err
	}

	var (
		reader  = bytes.NewReader([]byte(podcast.String()))
		xmlName = fmt.Sprintf("%s.xml", feedConfig.ID)
	)

	if _, err := u.fs.Create(ctx, xmlName, reader); err != nil {
		return errors.Wrap(err, "failed to upload new XML feed")
	}

	return nil
}

func (u *Manager) buildOPML(ctx context.Context) error {
	// Build OPML with data received from builder
	log.Debug("building podcast OPML")
	opml, err := feed.BuildOPML(ctx, u.feeds, u.db, u.hostname)
	if err != nil {
		return err
	}

	var (
		reader  = bytes.NewReader([]byte(opml))
		xmlName = fmt.Sprintf("%s.opml", "podsync")
	)

	if _, err := u.fs.Create(ctx, xmlName, reader); err != nil {
		return errors.Wrap(err, "failed to upload OPML")
	}

	return nil
}

func (u *Manager) cleanup(ctx context.Context, feedConfig *feed.Config) error {
	var (
		feedID = feedConfig.ID
		logger = log.WithField("feed_id", feedID)
		count  = feedConfig.Clean.KeepLast
		list   []*model.Episode
		result *multierror.Error
	)

	if count < 1 {
		logger.Info("nothing to clean")
		return nil
	}

	logger.WithField("count", count).Info("running cleaner")
	if err := u.db.WalkEpisodes(ctx, feedConfig.ID, func(episode *model.Episode) error {
		if episode.Status == model.EpisodeDownloaded {
			list = append(list, episode)
		}
		return nil
	}); err != nil {
		return err
	}

	if count > len(list) {
		return nil
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].PubDate.After(list[j].PubDate)
	})

	for _, episode := range list[count:] {
		logger.WithField("episode_id", episode.ID).Infof("deleting %q", episode.Title)

		var (
			episodeName = feed.EpisodeName(feedConfig, episode)
			path        = fmt.Sprintf("%s/%s", feedConfig.ID, episodeName)
		)

		if err := u.fs.Delete(ctx, path); err != nil {
			result = multierror.Append(result, errors.Wrapf(err, "failed to delete episode: %s", episode.ID))
			continue
		}

		if err := u.db.UpdateEpisode(feedID, episode.ID, func(episode *model.Episode) error {
			episode.Status = model.EpisodeCleaned
			episode.Title = ""
			episode.Description = ""
			return nil
		}); err != nil {
			result = multierror.Append(result, errors.Wrapf(err, "failed to set state for cleaned episode: %s", episode.ID))
			continue
		}
	}

	return result.ErrorOrNil()
}
