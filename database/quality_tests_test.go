package database

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestQualityTestSlotsAreAtomicAndReleaseOnlyAfterCompletion(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "quality.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var admitted []QualityTestJob
	for i := int64(1); i <= 18; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			job, err := db.CreateQualityTestJob(ctx, QualityTestJob{AccountID: id, AccountName: "snapshot", Model: "gpt-5.5", ReasoningEffort: "high", Prompt: "HTML"})
			if err == nil {
				mu.Lock()
				admitted = append(admitted, *job)
				mu.Unlock()
			} else if !errors.Is(err, ErrQualityTestCapacity) {
				t.Errorf("admission: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if len(admitted) != 3 {
		t.Fatalf("admitted %d jobs, want exactly 3", len(admitted))
	}
	job := admitted[0]
	if _, err := db.CreateQualityTestJob(ctx, QualityTestJob{AccountID: job.AccountID}); !errors.Is(err, ErrQualityTestAccountBusy) {
		t.Fatalf("duplicate: %v", err)
	}
	if err := db.CancelQualityTest(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateQualityTestJob(ctx, QualityTestJob{AccountID: 100}); !errors.Is(err, ErrQualityTestCapacity) {
		t.Fatalf("cancel released capacity too early: %v", err)
	}
	job.Output = "<html><svg/></html>"
	job.Status = "completed"
	if err := db.SaveQualityTestProgress(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishQualityTest(ctx, job); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetQualityTestJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "stopped" || stored.Output != job.Output || stored.CompletedAt == nil {
		t.Fatalf("cancel lost to completion: %+v", stored)
	}
	if _, err := db.CreateQualityTestJob(ctx, QualityTestJob{AccountID: 100}); err != nil {
		t.Fatal(err)
	}
}

func TestQualityTestRecordsPersistWithMetadataAndExcludeOutputFromLists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	job, err := db.CreateQualityTestJob(ctx, QualityTestJob{AccountID: 42, AccountName: "Historical name", PlanType: "pro", Channel: "codex", Model: "gpt-5.5", ReasoningEffort: "high", Prompt: "line 1\nline 2"})
	if err != nil {
		t.Fatal(err)
	}
	job.Output = "<html>鹈鹕</html>"
	job.Status = "completed"
	job.DurationMS = 1234
	if err := db.FinishQualityTest(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stored, err := db.GetQualityTestJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Output != job.Output || stored.AccountName != "Historical name" || stored.ReasoningEffort != "high" || stored.Model != "gpt-5.5" || stored.CreatedAt.IsZero() || stored.CompletedAt == nil || stored.DurationMS != 1234 {
		t.Fatalf("metadata/result did not persist: %+v", stored)
	}
	page, err := db.ListQualityTests(ctx, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.ActiveJobs) != 0 || page.Jobs[0].Output != "" || page.Jobs[0].Prompt != "" {
		t.Fatalf("unexpected history: %+v", page)
	}
	active, err := db.CreateQualityTestJob(ctx, QualityTestJob{AccountID: 43})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ExpireQualityTests(ctx, active.DeadlineAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	active, err = db.GetQualityTestJob(ctx, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active.Status != "interrupted" || active.CompletedAt == nil {
		t.Fatalf("orphan was not recovered: %+v", active)
	}
}
