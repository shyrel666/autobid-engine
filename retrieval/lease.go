package retrieval

import (
	"context"
	"time"
)

const jobLease = 20 * time.Second

// Stop work on a lost lease; a restarted coordinator can reclaim it after 20 seconds.
func (c *Coordinator) heartbeat(ctx context.Context, tenant string, job, token int64) (context.Context, func()) {
	work, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(jobLease / 3)
		defer tick.Stop()
		for {
			select {
			case <-work.Done():
				return
			case <-tick.C:
				if c.Service.DB.RenewRetrievalJob(work, tenant, job, token, jobLease) != nil {
					cancel()
					return
				}
			}
		}
	}()
	return work, func() { cancel(); <-done }
}
