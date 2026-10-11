package media

import (
	"context"
	"time"
)

func (e *Exporter) runPreparation(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-e.prepareQueue:
			e.prepareQueuedJob(ctx, job)
		}
	}
}

func (e *Exporter) prepareQueuedJob(parent context.Context, job exportJob) {
	// 准备与编码分开排队，避免一次长导出让后续任务长期占用录像快照。
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	ctx, cancelSpace := context.WithCancelCause(ctx)
	defer cancelSpace(nil)
	e.mu.Lock()
	task, ok := e.tasks[job.id]
	if !ok || task.State == ExportCanceled {
		delete(e.releases, job.id)
		e.mu.Unlock()
		job.release()
		return
	}
	task.State, task.Phase = ExportRunning, "preparing"
	e.tasks[job.id] = task
	e.cancels[job.id] = cancel
	e.mu.Unlock()
	preparing := true
	defer func() {
		if preparing {
			e.mu.Lock()
			delete(e.cancels, job.id)
			e.mu.Unlock()
		}
	}()
	stopSpace := e.watchSystemSpace(ctx, cancelSpace)
	defer stopSpace()
	copyCtx, cancelCopy := context.WithCancel(ctx)
	stop := func() bool { return true }
	if job.source.MediaContext != nil {
		stop = context.AfterFunc(job.source.MediaContext, cancelCopy)
		if job.source.MediaContext.Err() != nil {
			cancelCopy()
		}
	}
	lease, releaseSpace, err := e.stageSource(copyCtx, &job)
	stop()
	if err == nil {
		err = copyCtx.Err()
	}
	cancelCopy()
	err = exportContextError(ctx, err)
	if err != nil {
		if lease != nil {
			lease.Release()
		}
		if releaseSpace != nil {
			releaseSpace()
		}
		job.release()
		e.finishJob(job.id, task, err)
		e.mu.Lock()
		delete(e.releases, job.id)
		e.mu.Unlock()
		e.pruneFinished()
		return
	}
	e.mu.Lock()
	e.cacheLeases[job.id] = lease
	e.outputReservations[job.id] = releaseSpace
	e.preparedSources[job.id] = job.source
	current := e.tasks[job.id]
	if current.State != ExportCanceled {
		current.State, current.Phase = ExportQueued, "prepared"
		e.tasks[job.id] = current
	}
	delete(e.cancels, job.id)
	e.mu.Unlock()
	preparing = false
	if err := e.persistState(); err != nil {
		job.release()
		e.finishJob(job.id, task, err)
		e.discardPreparedJob(job.id)
		return
	}
	job.release()
	if current.State == ExportCanceled {
		e.discardPreparedJob(job.id)
		return
	}
	select {
	case e.queue <- job:
	case <-parent.Done():
		// 已暂存计划保留给下次启动；解除进程内持有，不保留失效快照。
		return
	}
}

func (e *Exporter) discardPreparedJob(id string) {
	e.mu.Lock()
	lease := e.cacheLeases[id]
	reservation := e.outputReservations[id]
	delete(e.cacheLeases, id)
	delete(e.outputReservations, id)
	delete(e.preparedSources, id)
	delete(e.releases, id)
	e.mu.Unlock()
	if lease != nil {
		lease.Release()
	}
	if reservation != nil {
		reservation()
	}
}

func (e *Exporter) releaseStoppedPreparation() {
	e.mu.Lock()
	ids := make([]string, 0)
	for id := range e.cacheLeases {
		if e.tasks[id].State != ExportRunning {
			ids = append(ids, id)
		}
	}
	e.mu.Unlock()
	for _, id := range ids {
		e.mu.Lock()
		lease := e.cacheLeases[id]
		reservation := e.outputReservations[id]
		delete(e.cacheLeases, id)
		delete(e.outputReservations, id)
		delete(e.releases, id)
		if e.tasks[id].State != ExportQueued {
			delete(e.preparedSources, id)
		}
		e.mu.Unlock()
		if lease != nil {
			lease.Release()
		}
		if reservation != nil {
			reservation()
		}
	}
}
