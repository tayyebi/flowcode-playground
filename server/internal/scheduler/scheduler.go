package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/apps"
	"github.com/tayyebi/flowcode-playground/server/internal/engine"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

// Scheduler periodically fires due time-driven triggers through the shared
// engine, records an execution row for each, and best-effort extracts any
// `store set` calls from the trace into the KV log. Runs are attributed to
// the project's owner so quotas and app-call records stay meaningful.
type Scheduler struct {
	Store  *store.Store
	Engine *engine.Engine

	// Tick is how often due triggers are checked. 15s is plenty of
	// resolution for interval schedules measured in minutes+ and for daily
	// HH:MM schedules.
	Tick time.Duration
	// RetentionInterval is how often old execution rows are pruned. A
	// 1-minute trigger produces 1440 rows/day/project; without pruning this
	// table grows unbounded.
	RetentionInterval       time.Duration
	RetentionKeepPerProject int

	// Now is injectable so tests don't have to sleep through real time.
	Now func() time.Time
}

// New builds a Scheduler with sensible defaults.
func New(st *store.Store, eng *engine.Engine) *Scheduler {
	return &Scheduler{
		Store:                   st,
		Engine:                  eng,
		Tick:                    15 * time.Second,
		RetentionInterval:       time.Hour,
		RetentionKeepPerProject: 500,
		Now:                     time.Now,
	}
}

// Run ticks until done is closed. Intended to be started as `go sch.Run(done)`
// from main, matching ratelimit.go's own ticker-based sweep goroutine.
func (sch *Scheduler) Run(done <-chan struct{}) {
	tick := time.NewTicker(sch.Tick)
	defer tick.Stop()
	retention := time.NewTicker(sch.RetentionInterval)
	defer retention.Stop()

	for {
		select {
		case <-done:
			return
		case <-tick.C:
			sch.fireDue(context.Background())
		case <-retention.C:
			sch.prune(context.Background())
		}
	}
}

func (sch *Scheduler) fireDue(ctx context.Context) {
	now := sch.Now()
	due, err := sch.Store.WSDueTriggers(ctx, FormatTime(now))
	if err != nil {
		log.Printf("scheduler: list due triggers: %v", err)
		return
	}
	for _, t := range due {
		sch.fire(ctx, t, now)
	}
}

func (sch *Scheduler) fire(ctx context.Context, t *store.Trigger, now time.Time) {
	source, err := sch.resolveSource(ctx, t)
	if err != nil {
		log.Printf("scheduler: trigger %d: resolve source: %v", t.ID, err)
		sch.reschedule(ctx, t, now)
		return
	}

	actor := apps.Actor{}
	if p, perr := sch.Store.GetWSProject(ctx, t.ProjectID); perr == nil {
		actor.ProjectID = p.ID
		if p.CreatedBy > 0 {
			actor.UserID = p.CreatedBy
			if u, uerr := sch.Store.GetUser(ctx, p.CreatedBy); uerr == nil {
				actor.Email = u.Email
			}
		}
	}

	result, runErr := sch.Engine.RunWithActor(ctx, source, actor)
	finished := sch.Now()

	fileID := t.FileID
	var actorID *int64
	if actor.UserID > 0 {
		id := actor.UserID
		actorID = &id
	}
	exec, recErr := sch.Store.RecordWSSExecution(ctx, store.NewExecution{
		ProjectID: t.ProjectID, FileID: &fileID, FileName: t.FileName, VersionID: t.VersionID,
		Source: "trigger", TriggerID: &t.ID,
		StartedAt: FormatTime(now), FinishedAt: FormatTime(finished),
		Summary: result.Summary(), Err: runErr,
	}, actorID)
	if recErr != nil {
		log.Printf("scheduler: trigger %d: record execution: %v", t.ID, recErr)
	} else {
		if result != nil && result.Run != nil {
			for k, v := range store.ExtractStoreSets(result.Run.Stderr) {
				if err := sch.Store.UpsertWSKV(ctx, t.ProjectID, k, v, &exec.ID); err != nil {
					log.Printf("scheduler: trigger %d: upsert kv %q: %v", t.ID, k, err)
				}
			}
		}
		sch.recordAppCalls(ctx, exec.ID, actorID, result)
	}

	sch.reschedule(ctx, t, now)
}

func (sch *Scheduler) recordAppCalls(ctx context.Context, execID int64, userID *int64, result *engine.Result) {
	if result == nil || len(result.AppCalls) == 0 {
		return
	}
	calls := []*store.AppCall{}
	for _, c := range result.AppCalls {
		switch c.Name {
		case "MailApp.sendEmail", "MailApp.read", "UrlFetchApp.fetch":
		default:
			continue // logger lives in Loki only
		}
		app := "http"
		if c.Name == "MailApp.sendEmail" || c.Name == "MailApp.read" {
			app = "mail"
		}
		calls = append(calls, &store.AppCall{
			ExecutionID: execID, UserID: userID, App: app, Name: c.Name,
			Status: c.Status, Request: c.Request, Response: c.Response, DurationMs: c.DurationMs,
		})
	}
	if len(calls) > 0 {
		if err := sch.Store.RecordAppCalls(ctx, calls); err != nil {
			log.Printf("scheduler: record app calls: %v", err)
		}
	}
}

func (sch *Scheduler) reschedule(ctx context.Context, t *store.Trigger, now time.Time) {
	prev, err := ParseTime(t.NextRunAt)
	if err != nil {
		prev = now
	}
	interval := 0
	if t.IntervalSeconds != nil {
		interval = *t.IntervalSeconds
	}
	next, err := AdvancePastNow(prev, now, t.ScheduleType, interval, t.DailyTimeUTC)
	if err != nil {
		log.Printf("scheduler: trigger %d: compute next run: %v; disabling to avoid a fire-loop", t.ID, err)
		sch.Store.SetWSTriggerEnabled(ctx, t.ID, false)
		return
	}
	if err := sch.Store.RecordWSTriggerRun(ctx, t.ID, FormatTime(now), FormatTime(next)); err != nil {
		log.Printf("scheduler: trigger %d: record run: %v", t.ID, err)
	}
}

// resolveSource reads the exact source a trigger should run: a pinned
// version's frozen snapshot, or the file's current live content.
func (sch *Scheduler) resolveSource(ctx context.Context, t *store.Trigger) (string, error) {
	if t.VersionID != nil {
		return sch.Store.GetWSVersionFileContent(ctx, *t.VersionID, t.FileName)
	}
	f, err := sch.Store.GetWSFile(ctx, t.ProjectID, t.FileName)
	if err != nil {
		return "", err
	}
	return f.Content, nil
}

func (sch *Scheduler) prune(ctx context.Context) {
	n, err := sch.Store.PruneWSExecutions(ctx, sch.RetentionKeepPerProject)
	if err != nil {
		log.Printf("scheduler: prune executions: %v", err)
		return
	}
	if n > 0 {
		log.Printf("scheduler: pruned %d old execution rows", n)
	}
}
