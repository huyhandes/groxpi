package index

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/huyhandes/groxpi/internal/config"
)

const (
	primaryURL = "https://primary.test/simple"
	extraURL   = "https://extra.test/simple"
)

// negativeConfig has one extra index with a shorter TTL than the primary, so
// the shortest TTL in the resolution order is the extra's.
func negativeConfig() *config.Config {
	return &config.Config{
		IndexURL:       primaryURL,
		IndexTTL:       time.Hour,
		ExtraIndexURLs: []string{extraURL},
		ExtraIndexTTLs: []time.Duration{3 * time.Minute},
		IndexCacheSize: 1 << 20,
	}
}

// TestResolve_CachesAMissForTheShortestTTL pins A8: a package no index has is
// remembered, so a retry does not ask every upstream again, until the shortest
// TTL in the resolution order has passed.
func TestResolve_CachesAMissForTheShortestTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := newFakeUpstream()
		svc := newFakeService(t, negativeConfig(), up)

		for range 2 {
			_, err := svc.Resolve(t.Context(), "Ghost_Pkg")
			require.ErrorIs(t, err, ErrNotFound)
		}
		assert.Equal(t, 1, up.queried(extraURL, "ghost-pkg"), "a cached miss must not query the extra again")
		assert.Equal(t, 1, up.queried(primaryURL, "ghost-pkg"), "a cached miss must not query the primary again")

		time.Sleep(3 * time.Minute)
		_, err := svc.Resolve(t.Context(), "ghost-pkg")
		require.ErrorIs(t, err, ErrNotFound)
		assert.Equal(t, 1, up.queried(primaryURL, "ghost-pkg"), "the miss is still fresh at the shortest TTL")

		time.Sleep(time.Nanosecond)
		_, err = svc.Resolve(t.Context(), "ghost-pkg")
		require.ErrorIs(t, err, ErrNotFound)
		assert.Equal(t, 2, up.queried(extraURL, "ghost-pkg"), "past the shortest TTL the miss is asked again")
		assert.Equal(t, 2, up.queried(primaryURL, "ghost-pkg"))
	})
}

// TestResolve_ErrorsAndEmptyListsAreNotCached pins what negative caching leaves
// alone: an unreachable index and an empty list are transient-looking answers,
// so the next request goes upstream again.
func TestResolve_ErrorsAndEmptyListsAreNotCached(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := newFakeUpstream()
		up.set(extraURL, "broken", nil, errors.New("connection refused"))
		up.set(primaryURL, "empty", []FileInfo{}, nil)
		svc := newFakeService(t, negativeConfig(), up)

		for range 2 {
			_, err := svc.Resolve(t.Context(), "broken")
			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrNotFound)
		}
		assert.Equal(t, 2, up.queried(extraURL, "broken"), "an upstream error must not be cached")

		for range 2 {
			entry, err := svc.Resolve(t.Context(), "empty")
			require.NoError(t, err)
			assert.Empty(t, entry.Files)
		}
		assert.Equal(t, 2, up.queried(primaryURL, "empty"), "an empty list must not be cached")
	})
}

// TestInvalidate_ClearsACachedMiss pins the operator's way out: DELETE
// /cache/{package} drops a cached miss, so a package just published shows up on
// the next request instead of after the TTL.
func TestInvalidate_ClearsACachedMiss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := newFakeUpstream()
		svc := newFakeService(t, negativeConfig(), up)

		_, err := svc.Resolve(t.Context(), "fresh")
		require.ErrorIs(t, err, ErrNotFound)

		up.set(extraURL, "fresh", []FileInfo{{Name: "fresh-1.0.tar.gz"}}, nil)
		_, err = svc.Resolve(t.Context(), "fresh")
		require.ErrorIs(t, err, ErrNotFound, "the miss is cached, so the new upload is not seen yet")

		svc.Invalidate("Fresh")

		entry, err := svc.Resolve(t.Context(), "fresh")
		require.NoError(t, err)
		assert.Equal(t, "fresh-1.0.tar.gz", entry.Files[0].Name)
		assert.Equal(t, 2, up.queried(extraURL, "fresh"))
	})
}
