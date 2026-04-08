package scraper

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/gosom/google-maps-scraper/gmaps"
	"github.com/gosom/google-maps-scraper/internal/jsonbsanitize"
	"github.com/gosom/google-maps-scraper/log"
	"github.com/gosom/scrapemate"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FlushResult is sent to the River worker when results are flushed to DB.
type FlushResult struct {
	ResultCount int
	Err         error
}

const flushBatchSize = 50

type trackedJob struct {
	jobID      string
	entries    []*gmaps.Entry
	completion chan FlushResult
	riverJobID int64
	keyword    string
	startedAt  time.Time
	totalSaved int
}

// SaveFunc persists results. The default writes to PostgreSQL;
// tests can supply a lightweight substitute via NewCentralWriter.
type SaveFunc func(ctx context.Context, riverJobID int64, keyword string, entries []*gmaps.Entry) error

var _ scrapemate.ResultWriter = (*CentralWriter)(nil)

// CentralWriter tracks exactly one in-flight River job, receives ScrapeMate
// results, and flushes them to the database when the exit monitor signals done.
type CentralWriter struct {
	mu      sync.Mutex
	current *trackedJob

	save           SaveFunc
	OnResultsSaved func(count int)
}

// NewCentralWriter creates a new CentralWriter.
// Pass nil for saveFn to use the default PostgreSQL saver built from db.
func NewCentralWriter(db *pgxpool.Pool, saveFn SaveFunc) *CentralWriter {
	if saveFn == nil {
		saveFn = pgSave(db)
	}

	return &CentralWriter{save: saveFn}
}

// RegisterJob registers the active River job and returns a completion channel
// that receives the flush result.
func (cw *CentralWriter) RegisterJob(jobID string, riverJobID int64, keyword string) <-chan FlushResult {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	ch := make(chan FlushResult, 1)
	cw.current = &trackedJob{
		jobID:      jobID,
		completion: ch,
		riverJobID: riverJobID,
		keyword:    keyword,
		startedAt:  time.Now(),
	}

	log.Debug("registered scrape job", "job_id", jobID, "river_job_id", riverJobID)

	return ch
}

// AddResult appends an entry for the currently tracked job.
// When the in-memory buffer hits flushBatchSize, a batch is saved to DB
// and the buffer is cleared to keep memory bounded.
func (cw *CentralWriter) AddResult(jobID string, entry *gmaps.Entry) {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	if cw.current == nil || cw.current.jobID != jobID {
		return
	}

	cw.current.entries = append(cw.current.entries, entry)

	if len(cw.current.entries) >= flushBatchSize {
		cw.saveBatchLocked()
	}
}

// saveBatchLocked persists the current in-memory entries to DB and clears
// the buffer. Must be called while cw.mu is held.
func (cw *CentralWriter) saveBatchLocked() {
	j := cw.current
	if j == nil || len(j.entries) == 0 {
		return
	}

	for _, entry := range j.entries {
		jsonbsanitize.StripNULFromEntry(entry)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := cw.save(ctx, j.riverJobID, j.keyword, j.entries)
	cancel()

	if err != nil {
		log.Error("failed to save batch",
			"job_id", j.jobID,
			"river_job_id", j.riverJobID,
			"batch_size", len(j.entries),
			"error", err,
		)
		return
	}

	j.totalSaved += len(j.entries)
	j.entries = j.entries[:0]

	if cw.OnResultsSaved != nil {
		cw.OnResultsSaved(j.totalSaved)
	}
}

// MarkDone is called by the exit monitor when a job is complete.
func (cw *CentralWriter) MarkDone(jobID string) {
	cw.Flush(jobID)
}

// ForceFlush immediately flushes results for a job (used on timeout/shutdown).
func (cw *CentralWriter) ForceFlush(jobID string) {
	cw.Flush(jobID)
}

// Discard drops the tracked job without persisting results.
func (cw *CentralWriter) Discard(jobID string) {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	if cw.current != nil && cw.current.jobID == jobID {
		cw.current = nil
	}
}

// TrackedJobs returns how many jobs are currently tracked in-memory.
func (cw *CentralWriter) TrackedJobs() int {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	if cw.current == nil {
		return 0
	}

	return 1
}

// FlushQueueDepth is kept for health endpoint compatibility.
func (cw *CentralWriter) FlushQueueDepth() int {
	return 0
}

// Flush saves any remaining results to DB, signals completion, and clears tracked state.
// Idempotent: a second call for the same jobID is a no-op.
func (cw *CentralWriter) Flush(jobID string) {
	cw.mu.Lock()
	j := cw.current

	if j == nil || j.jobID != jobID {
		cw.mu.Unlock()
		return
	}

	cw.current = nil
	cw.mu.Unlock()

	var err error

	// Save any remaining entries not yet flushed by batch saves
	if len(j.entries) > 0 {
		for _, entry := range j.entries {
			jsonbsanitize.StripNULFromEntry(entry)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = cw.save(ctx, j.riverJobID, j.keyword, j.entries)
		cancel()

		if err != nil {
			log.Error("failed to save results",
				"job_id", j.jobID,
				"river_job_id", j.riverJobID,
				"error", err,
			)
		}
	}

	totalCount := j.totalSaved + len(j.entries)

	if err == nil && cw.OnResultsSaved != nil && totalCount > 0 {
		cw.OnResultsSaved(totalCount)
	}

	j.completion <- FlushResult{ResultCount: totalCount, Err: err}

	log.Debug("flushed scrape job",
		"job_id", j.jobID,
		"river_job_id", j.riverJobID,
		"result_count", totalCount,
		"duration_ms", time.Since(j.startedAt).Milliseconds(),
		"save_error", err,
	)
}

// Run processes results from the ScrapeMate scraper and buffers them for the
// currently registered River job.
func (cw *CentralWriter) Run(ctx context.Context, in <-chan scrapemate.Result) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result, ok := <-in:
			if !ok {
				return nil
			}

			cw.processResult(result)
		}
	}
}

func (cw *CentralWriter) processResult(result scrapemate.Result) {
	jobID := ""
	if result.Job != nil {
		jobID = result.Job.GetID()
	}

	if entry, ok := result.Data.(*gmaps.Entry); ok {
		entryJobID := entry.ID
		if entryJobID == "" {
			entryJobID = jobID
		}

		cw.AddResult(entryJobID, entry)
		cw.markCompletedFromResult(result, 1)

		return
	}

	if entries, ok := result.Data.([]*gmaps.Entry); ok {
		for _, entry := range entries {
			entry.ID = jobID
			cw.AddResult(jobID, entry)
		}

		cw.markCompletedFromResult(result, len(entries))
	}
}

func (cw *CentralWriter) markCompletedFromResult(result scrapemate.Result, count int) {
	if count <= 0 || result.Job == nil {
		return
	}

	switch job := result.Job.(type) {
	case *gmaps.PlaceJob:
		if job.WriterManagedCompletion && job.ExitMonitor != nil {
			job.ExitMonitor.IncrPlacesCompleted(count)
		}
	case *gmaps.EmailExtractJob:
		if job.WriterManagedCompletion && job.ExitMonitor != nil {
			job.ExitMonitor.IncrPlacesCompleted(count)
		}
	case *gmaps.SearchJob:
		if job.WriterManagedCompletion && job.ExitMonitor != nil {
			job.ExitMonitor.IncrPlacesCompleted(count)
		}
	}
}

// pgSave returns a SaveFunc that writes to the scrape_results table.
// On conflict it appends the new batch to existing results instead of replacing,
// so batch-flushed entries accumulate correctly.
func pgSave(db *pgxpool.Pool) SaveFunc {
	return func(ctx context.Context, riverJobID int64, keyword string, entries []*gmaps.Entry) error {
		resultsJSON, err := json.Marshal(entries)
		if err != nil {
			return err
		}

		q := `INSERT INTO scrape_results (job_id, keyword, results, result_count)
			VALUES ($1, $2, $3::jsonb, $4)
			ON CONFLICT (job_id) DO UPDATE SET
				results = scrape_results.results || $3::jsonb,
				result_count = scrape_results.result_count + $4`

		_, err = db.Exec(ctx, q, riverJobID, keyword, resultsJSON, len(entries))

		return err
	}
}
