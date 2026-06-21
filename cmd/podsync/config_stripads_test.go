package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/feed"
)

func TestLoadConfigStripAds(t *testing.T) {
	const file = `
[server]
data_dir = "test/data/"

[database]
dir = "/home/user/db/"

[feeds]
  [feeds.TWiT]
  url = "https://youtube.com/@ThisWeekinTech"
  format = "video"
  [feeds.TWiT.strip_ads]
  enabled = true
  aggressiveness = "aggressive"

  [feeds.Plain]
  url = "https://youtube.com/watch?v=abc"
  format = "video"

  [feeds.Typo]
  url = "https://youtube.com/watch?v=def"
  format = "video"
  [feeds.Typo.strip_ads]
  enabled = true
  aggressiveness = "nonsense"
`
	path := setup(t, file)
	defer os.Remove(path)

	config, err := LoadConfig(path)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Explicit, valid aggressiveness is preserved.
	twit := config.Feeds["TWiT"]
	require.NotNil(t, twit)
	assert.True(t, twit.StripAds.Enabled)
	assert.Equal(t, feed.AggressivenessAggressive, twit.StripAds.Aggressiveness)

	// No strip_ads block -> disabled, but aggressiveness still normalised to balanced.
	plain := config.Feeds["Plain"]
	require.NotNil(t, plain)
	assert.False(t, plain.StripAds.Enabled)
	assert.Equal(t, feed.AggressivenessBalanced, plain.StripAds.Aggressiveness)

	// Unknown aggressiveness coerces to balanced (a typo must never widen cuts).
	typo := config.Feeds["Typo"]
	require.NotNil(t, typo)
	assert.True(t, typo.StripAds.Enabled)
	assert.Equal(t, feed.AggressivenessBalanced, typo.StripAds.Aggressiveness)
}

func TestStripAdsNormalize(t *testing.T) {
	cases := map[string]string{
		"":             feed.AggressivenessBalanced,
		"balanced":     feed.AggressivenessBalanced,
		"conservative": feed.AggressivenessConservative,
		"aggressive":   feed.AggressivenessAggressive,
		"AGGRESSIVE":   feed.AggressivenessBalanced, // case-sensitive: unknown -> balanced
		"weird":        feed.AggressivenessBalanced,
	}
	for in, want := range cases {
		s := feed.StripAds{Aggressiveness: in}
		s.Normalize()
		assert.Equalf(t, want, s.Aggressiveness, "Normalize(%q)", in)
	}
}
