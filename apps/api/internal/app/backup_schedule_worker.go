package app

import (
	"context"
	"time"
)

func (a *App) backupScheduleWorker(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := a.runDueBackupSchedules(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("backup schedule scan failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
