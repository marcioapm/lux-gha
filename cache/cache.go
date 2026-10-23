// Package cache is a GitHub Actions cache service (the Results cache API v2)
// backed by S3. Runners reach it as ACTIONS_RESULTS_URL; cache requests are
// served here, every other request is passed to the real Results service.
//
// S3 is the only state. Final entries are objects under a partitioned
// prefix, so a lookup is a HEAD or a prefix listing; uploads land in a
// separate staging prefix and become visible only on finalize.
package cache

import (
	"context"
	"time"
)

// New returns a Server storing entries in the S3 bucket of so.
func New(ctx context.Context, so S3Options, o Options) (*Server, error) {
	st, err := newS3Store(ctx, so)
	if err != nil {
		return nil, err
	}
	return newServer(st, o)
}

// RunReaper reaps once at start and then every ReapInterval until ctx ends.
func (s *Server) RunReaper(ctx context.Context) {
	t := time.NewTicker(s.o.ReapInterval)
	defer t.Stop()
	for {
		s.reaper.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
