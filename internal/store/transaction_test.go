package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nordicintel/nordicintel-storage-api/internal/domain"
)

var errForcedFailure = errors.New("forced checkpoint failure")

// state captures everything a caller can observe about one dataset, so a test
// can assert that a failed replacement changed nothing at all.
type state struct {
	summary domain.Summary
	cells   []domain.Cell
	codes   []string
}

func observe(t *testing.T, database *Postgres, provider, dataset domain.Code) state {
	t.Helper()
	ctx := t.Context()
	view, err := database.GetData(ctx, provider, dataset)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var codes []string
	for _, dimension := range view.Dimensions {
		for _, category := range dimension.Categories {
			codes = append(codes, dimension.Code.Spelling+"/"+category.Code.Spelling)
		}
	}
	return state{summary: view.Summary, cells: view.Cells, codes: codes}
}

func (s state) equals(other state) bool {
	if s.summary.CellCount != other.summary.CellCount ||
		s.summary.ValuedCellCount != other.summary.ValuedCellCount ||
		!s.summary.UpdatedAt.Equal(other.summary.UpdatedAt) ||
		string(s.summary.SourceStamp) != string(other.summary.SourceStamp) ||
		s.summary.ProviderCode != other.summary.ProviderCode ||
		s.summary.DatasetCode != other.summary.DatasetCode ||
		len(s.cells) != len(other.cells) ||
		strings.Join(s.codes, ",") != strings.Join(other.codes, ",") {
		return false
	}
	for i := range s.cells {
		if s.cells[i].Index != other.cells[i].Index {
			return false
		}
		if (s.cells[i].Numeric == nil) != (other.cells[i].Numeric == nil) {
			return false
		}
		if s.cells[i].Numeric != nil && *s.cells[i].Numeric != *other.cells[i].Numeric {
			return false
		}
	}
	return true
}

func (s state) String() string {
	return fmt.Sprintf("stamp=%s counts=%d/%d updated=%s codes=%v cells=%d",
		s.summary.SourceStamp, s.summary.ValuedCellCount, s.summary.CellCount,
		s.summary.UpdatedAt.Format(time.RFC3339Nano), s.codes, len(s.cells))
}

const originalBody = `{"source_stamp":{"generation":1},` +
	`"id":["sex","year"],"dimension":{"sex":{"index":{"M":0,"F":1}},"year":{"index":{"2024":0,"2025":1}}},` +
	`"value":[1,2,3,4]}`

// replacementBody is deliberately different in structure, codes, counts, and
// stamp, so any leaked fragment of it is detectable.
const replacementBody = `{"replace":true,"source_stamp":{"generation":2},` +
	`"id":["region"],"dimension":{"region":{"index":{"north":0,"south":1,"east":2}}},` +
	`"value":[7,8,9]}`

func TestFailedReplacementLeavesThePreviousStateIntact(t *testing.T) {
	checkpoints := []struct {
		name string
		hook func(*transactionCheckpoints) *func(context.Context) error
	}{
		{"before structure insertion", func(c *transactionCheckpoints) *func(context.Context) error {
			return &c.BeforeStructure
		}},
		{"before the observation COPY", func(c *transactionCheckpoints) *func(context.Context) error {
			return &c.BeforeCopy
		}},
		{"before commit", func(c *transactionCheckpoints) *func(context.Context) error {
			return &c.BeforeCommit
		}},
	}
	for _, tc := range checkpoints {
		t.Run(tc.name, func(t *testing.T) {
			database := openStore(t)
			ctx := t.Context()
			provider, dataset := code(t, "SCB"), code(t, "Population")
			if _, _, err := database.Replace(ctx, parse(t, "SCB", "Population", originalBody)); err != nil {
				t.Fatalf("create: %v", err)
			}
			before := observe(t, database, provider, dataset)

			hooks := &transactionCheckpoints{}
			*tc.hook(hooks) = func(context.Context) error { return errForcedFailure }
			database.checkpoints = hooks

			_, _, err := database.Replace(ctx, parse(t, "SCB", "Population", replacementBody))
			if !errors.Is(err, errForcedFailure) {
				t.Fatalf("error = %v, want the forced failure", err)
			}
			database.checkpoints = nil

			after := observe(t, database, provider, dataset)
			if !before.equals(after) {
				t.Fatalf("a failed replacement changed the dataset:\n before %v\n after  %v", before, after)
			}
			if strings.Contains(strings.Join(after.codes, ","), "region") {
				t.Fatalf("the failed replacement leaked its structure: %v", after.codes)
			}
		})
	}
}

func TestFailedCreationLeavesNoDataset(t *testing.T) {
	database := openStore(t)
	ctx := t.Context()
	database.checkpoints = &transactionCheckpoints{
		BeforeCopy: func(context.Context) error { return errForcedFailure },
	}
	_, _, err := database.Replace(ctx, parse(t, "SCB", "Population", originalBody))
	if !errors.Is(err, errForcedFailure) {
		t.Fatalf("error = %v, want the forced failure", err)
	}
	database.checkpoints = nil

	if _, err := database.GetSummary(ctx, code(t, "SCB"), code(t, "Population")); err != ErrNotFound {
		t.Fatalf("error = %v, want the dataset to be absent", err)
	}
	providers, err := database.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 0 {
		t.Fatalf("providers = %+v, want none after a rolled-back creation", providers)
	}
}

func TestCancellationDuringAReplacementRollsBack(t *testing.T) {
	database := openStore(t)
	provider, dataset := code(t, "SCB"), code(t, "Population")
	if _, _, err := database.Replace(t.Context(), parse(t, "SCB", "Population", originalBody)); err != nil {
		t.Fatalf("create: %v", err)
	}
	before := observe(t, database, provider, dataset)

	cancellable, cancel := context.WithCancel(t.Context())
	database.checkpoints = &transactionCheckpoints{
		BeforeCommit: func(ctx context.Context) error {
			cancel()
			return ctx.Err()
		},
	}
	_, _, err := database.Replace(cancellable, parse(t, "SCB", "Population", replacementBody))
	if err == nil {
		t.Fatal("the cancelled replacement reported success")
	}
	database.checkpoints = nil

	after := observe(t, database, provider, dataset)
	if !before.equals(after) {
		t.Fatalf("cancellation left a changed dataset:\n before %v\n after  %v", before, after)
	}
}

func TestReadersSeeTheOldStateUntilAReplacementCommits(t *testing.T) {
	database := openStore(t)
	provider, dataset := code(t, "SCB"), code(t, "Population")
	if _, _, err := database.Replace(t.Context(), parse(t, "SCB", "Population", originalBody)); err != nil {
		t.Fatalf("create: %v", err)
	}
	before := observe(t, database, provider, dataset)

	held := make(chan struct{})
	release := make(chan struct{})
	database.checkpoints = &transactionCheckpoints{
		BeforeCommit: func(context.Context) error {
			close(held)
			<-release
			return nil
		},
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := database.Replace(t.Context(), parse(t, "SCB", "Population", replacementBody))
		done <- err
	}()

	<-held
	// The writer is holding an uncommitted transaction with every change
	// already applied inside it. Concurrent readers must still see the
	// complete previous state.
	for range 3 {
		during := observe(t, database, provider, dataset)
		if !before.equals(during) {
			t.Fatalf("a reader saw uncommitted state:\n before %v\n during %v", before, during)
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("replacement: %v", err)
	}
	database.checkpoints = nil

	after := observe(t, database, provider, dataset)
	if before.equals(after) {
		t.Fatal("the committed replacement is not visible")
	}
	if after.summary.CellCount != 3 || strings.Join(after.codes, ",") == strings.Join(before.codes, ",") {
		t.Fatalf("the new state is incomplete: %v", after)
	}
	if string(after.summary.SourceStamp) == string(before.summary.SourceStamp) {
		t.Fatalf("the source stamp was not replaced: %s", after.summary.SourceStamp)
	}
}

func TestConcurrentCreateOnlyRequestsProduceOneWinner(t *testing.T) {
	database := openStore(t)
	const attempts = 8
	var group sync.WaitGroup
	results := make([]error, attempts)
	start := make(chan struct{})
	for i := range attempts {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, _, err := database.Replace(t.Context(), parse(t, "SCB", "Population", originalBody))
			results[i] = err
		}()
	}
	close(start)
	group.Wait()

	created, conflicts := 0, 0
	for i, err := range results {
		switch {
		case err == nil:
			created++
		case errors.Is(err, ErrDatasetExists):
			conflicts++
		default:
			t.Fatalf("attempt %d failed unexpectedly: %v", i, err)
		}
	}
	if created != 1 || conflicts != attempts-1 {
		t.Fatalf("%d creations and %d conflicts, want exactly one creation", created, conflicts)
	}
}

func TestConcurrentReplacementsSerializeAndTheLastCommitWins(t *testing.T) {
	database := openStore(t)
	ctx := t.Context()
	if _, _, err := database.Replace(ctx, parse(t, "SCB", "Population", originalBody)); err != nil {
		t.Fatalf("create: %v", err)
	}

	const writers = 6
	var group sync.WaitGroup
	start := make(chan struct{})
	stamps := make([]string, writers)
	for i := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			body := fmt.Sprintf(
				`{"replace":true,"source_stamp":{"writer":%d},"id":["w"],`+
					`"dimension":{"w":{"index":{"a":0,"b":1}}},"value":[%d,%d]}`, i, i, i)
			_, summary, err := database.Replace(t.Context(), parse(t, "SCB", "Population", body))
			if err != nil {
				t.Errorf("writer %d: %v", i, err)
				return
			}
			stamps[i] = string(summary.SourceStamp)
		}()
	}
	close(start)
	group.Wait()
	if t.Failed() {
		t.FailNow()
	}

	// Whatever the interleaving, the committed state must be exactly one
	// writer's complete submission.
	final := observe(t, database, code(t, "SCB"), code(t, "Population"))
	if final.summary.CellCount != 2 || final.summary.ValuedCellCount != 2 {
		t.Fatalf("final counts = %+v, want one writer's complete state", final.summary)
	}
	if len(final.cells) != 2 {
		t.Fatalf("final cells = %d, want 2", len(final.cells))
	}
	value := *final.cells[0].Numeric
	if *final.cells[1].Numeric != value {
		t.Fatalf("the final state mixes writers: %v and %v", value, *final.cells[1].Numeric)
	}
	if string(final.summary.SourceStamp) != fmt.Sprintf(`{"writer": %d}`, int(value)) &&
		string(final.summary.SourceStamp) != fmt.Sprintf(`{"writer":%d}`, int(value)) {
		t.Fatalf("the stamp %s does not belong to the writer whose values survived (%v)",
			final.summary.SourceStamp, value)
	}
}

func TestConcurrentReplacementAndDeletionLeaveAConsistentState(t *testing.T) {
	database := openStore(t)
	provider, dataset := code(t, "SCB"), code(t, "Population")
	for attempt := range 10 {
		if _, _, err := database.Replace(t.Context(), parse(t, "SCB", "Population",
			strings.Replace(originalBody, `"source_stamp"`, `"replace":true,"source_stamp"`, 1))); err != nil {
			t.Fatalf("attempt %d seed: %v", attempt, err)
		}
		var group sync.WaitGroup
		group.Add(2)
		start := make(chan struct{})
		var replaceErr, deleteErr error
		go func() {
			defer group.Done()
			<-start
			_, _, replaceErr = database.Replace(t.Context(), parse(t, "SCB", "Population", replacementBody))
		}()
		go func() {
			defer group.Done()
			<-start
			deleteErr = database.Delete(t.Context(), provider, dataset)
		}()
		close(start)
		group.Wait()
		if replaceErr != nil {
			t.Fatalf("attempt %d replacement: %v", attempt, replaceErr)
		}
		if deleteErr != nil {
			t.Fatalf("attempt %d deletion: %v", attempt, deleteErr)
		}

		// Either the deletion committed last and the dataset is gone, or the
		// replacement committed last and the dataset is complete. A mixture is
		// never acceptable.
		view, err := database.GetData(t.Context(), provider, dataset)
		switch {
		case errors.Is(err, ErrNotFound):
			continue
		case err != nil:
			t.Fatalf("attempt %d read: %v", attempt, err)
		}
		if view.Summary.CellCount != 3 || len(view.Dimensions) != 1 ||
			len(view.Dimensions[0].Categories) != 3 || len(view.Cells) != 3 {
			t.Fatalf("attempt %d observed a partial dataset: counts=%+v dimensions=%+v cells=%d",
				attempt, view.Summary, view.Dimensions, len(view.Cells))
		}
	}
}

func TestIndependentDatasetsWriteConcurrently(t *testing.T) {
	database := openStore(t)
	const writers = 8
	var group sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, writers)
	for i := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, _, errs[i] = database.Replace(t.Context(),
				parse(t, "SCB", fmt.Sprintf("Dataset%d", i), originalBody))
		}()
	}
	close(start)
	group.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	providers, err := database.ListProviders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].DatasetCount != writers {
		t.Fatalf("providers = %+v, want one provider with %d datasets", providers, writers)
	}
}

func TestSameIdentityWritesShareOneAdvisoryLock(t *testing.T) {
	database := openStore(t)
	// Hold a replacement open and confirm the advisory lock is visible with the
	// key the specification documents.
	held := make(chan struct{})
	release := make(chan struct{})
	database.checkpoints = &transactionCheckpoints{
		BeforeCommit: func(context.Context) error {
			close(held)
			<-release
			return nil
		},
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := database.Replace(t.Context(), parse(t, "SCB", "Population", originalBody))
		done <- err
	}()
	<-held

	var locks int
	err := database.pool.QueryRow(t.Context(), `
		select count(*) from pg_locks
		where locktype = 'advisory' and objid = ($1::bigint & 4294967295)::oid
		  and ((($1::bigint >> 32) & 4294967295)::oid) = classid
	`, advisoryKey("scb", "population")).Scan(&locks)
	close(release)
	if waitErr := <-done; waitErr != nil {
		t.Fatalf("replacement: %v", waitErr)
	}
	database.checkpoints = nil
	if err != nil {
		t.Fatalf("inspect pg_locks: %v", err)
	}
	if locks == 0 {
		t.Fatal("no advisory lock was held for the dataset identity during a replacement")
	}
}

func TestDeleteWaitsForAnInFlightReplacement(t *testing.T) {
	database := openStore(t)
	provider, dataset := code(t, "SCB"), code(t, "Population")
	if _, _, err := database.Replace(t.Context(), parse(t, "SCB", "Population", originalBody)); err != nil {
		t.Fatalf("create: %v", err)
	}

	held := make(chan struct{})
	release := make(chan struct{})
	database.checkpoints = &transactionCheckpoints{
		BeforeCommit: func(context.Context) error {
			close(held)
			<-release
			return nil
		},
	}
	writeDone := make(chan error, 1)
	go func() {
		_, _, err := database.Replace(t.Context(), parse(t, "SCB", "Population", replacementBody))
		writeDone <- err
	}()
	<-held

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- database.Delete(t.Context(), provider, dataset) }()

	select {
	case err := <-deleteDone:
		t.Fatalf("the deletion completed while the replacement held the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	if err := <-writeDone; err != nil {
		t.Fatalf("replacement: %v", err)
	}
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("deletion: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the deletion never acquired the lock")
	}
	database.checkpoints = nil
	if _, err := database.GetSummary(t.Context(), provider, dataset); err != ErrNotFound {
		t.Fatalf("error = %v, want the dataset deleted", err)
	}
}

func TestProductionConstructionEnablesNoCheckpoints(t *testing.T) {
	database := openStore(t)
	if database.checkpoints != nil {
		t.Fatal("Open installed transaction checkpoints")
	}
	// The nil default must be safe on every path.
	if _, _, err := database.Replace(t.Context(), parse(t, "SCB", "Population", originalBody)); err != nil {
		t.Fatalf("replace: %v", err)
	}
}

func TestObservationsNeverOutliveTheirDataset(t *testing.T) {
	url := migratedDatabase(t)
	database, err := Open(t.Context(), url, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := t.Context()
	if _, _, err := database.Replace(ctx, parse(t, "SCB", "Population", originalBody)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := database.Replace(ctx, parse(t, "SCB", "Population", replacementBody)); err != nil {
		t.Fatal(err)
	}
	conn := connect(t, url)
	var orphans int64
	if err := conn.QueryRow(ctx, `
		select count(*) from storage.observations o
		left join storage.datasets d on d.dataset_id = o.dataset_id
		where d.dataset_id is null
	`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Fatalf("%d observations outlived their dataset", orphans)
	}
	var beyondCube int64
	if err := conn.QueryRow(ctx, `
		select count(*) from storage.observations o
		join storage.datasets d on d.dataset_id = o.dataset_id
		where o.cell_index >= d.cell_count
	`).Scan(&beyondCube); err != nil {
		t.Fatal(err)
	}
	if beyondCube != 0 {
		t.Fatalf("%d observations sit outside their dataset's cube", beyondCube)
	}
	var rows int64
	if err := conn.QueryRow(ctx, `select count(*) from storage.observations`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Fatalf("observations = %d, want only the current replacement's three rows", rows)
	}
}
