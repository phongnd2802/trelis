package repository

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
	"github.com/phongnd2802/trelis/internal/types"
)

// compactJSON renders the event detail without the spacing jsonb adds, so a
// test can compare it with the object it inserted.
func compactJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var buf json.RawMessage
	if err := json.Unmarshal(raw, &buf); err != nil {
		t.Fatalf("detail %s is not JSON: %v", raw, err)
	}
	var out []byte
	out, err := json.Marshal(buf)
	if err != nil {
		t.Fatalf("marshal detail %s: %v", raw, err)
	}
	return string(out)
}

func TestIngestEventsAreListedAfterASeqInSeqOrder(t *testing.T) {
	s, ids := seeded(t)
	ctx := context.Background()
	events := NewIngestEventRepository(s)
	doc, other := document(t, s, ids["alice"]), document(t, s, ids["bob"])

	first, err := events.InsertIngestEvent(ctx, &types.IngestEvent{
		DocumentID: doc.ID,
		Type:       types.IngestEventParseStarted,
		Detail:     json.RawMessage(`{"attempt":1}`),
	})
	if err != nil {
		t.Fatalf("insert parse_started: %v", err)
	}
	if first.Seq <= 0 || first.DocumentID != doc.ID || first.Type != types.IngestEventParseStarted ||
		first.CreatedAt.IsZero() {
		t.Errorf("stored event = %+v, want its seq, Document, type and created_at filled in", first)
	}
	if got := compactJSON(t, first.Detail); got != `{"attempt":1}` {
		t.Errorf("detail = %s, want {\"attempt\":1}", got)
	}

	second, err := events.InsertIngestEvent(ctx, &types.IngestEvent{
		DocumentID: doc.ID,
		Type:       types.IngestEventParseFinished,
		Detail:     json.RawMessage(`{"chunks":3}`),
	})
	if err != nil {
		t.Fatalf("insert parse_finished: %v", err)
	}
	if _, err := events.InsertIngestEvent(ctx, &types.IngestEvent{
		DocumentID: other.ID,
		Type:       types.IngestEventFailed,
		Detail:     json.RawMessage(`{"reason":"the Document could not be ingested"}`),
	}); err != nil {
		t.Fatalf("insert another Document's event: %v", err)
	}
	third, err := events.InsertIngestEvent(ctx, &types.IngestEvent{
		DocumentID: doc.ID,
		Type:       types.IngestEventCompleted,
		Detail:     json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("insert completed: %v", err)
	}
	if first.Seq >= second.Seq || second.Seq >= third.Seq {
		t.Errorf("seqs = %d, %d, %d, want strictly increasing", first.Seq, second.Seq, third.Seq)
	}

	got, err := events.ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil {
		t.Fatalf("list all events: %v", err)
	}
	want := []string{types.IngestEventParseStarted, types.IngestEventParseFinished, types.IngestEventCompleted}
	if len(got) != len(want) {
		t.Fatalf("listed %d events, want %d: %+v", len(got), len(want), got)
	}
	for i, event := range got {
		if event.Type != want[i] || event.DocumentID != doc.ID {
			t.Errorf("event %d = %s of %s, want %s of the Document", i, event.Type, event.DocumentID, want[i])
		}
		if event.Seq != []int64{first.Seq, second.Seq, third.Seq}[i] {
			t.Errorf("event %d has seq %d, want %d", i, event.Seq, []int64{first.Seq, second.Seq, third.Seq}[i])
		}
	}
	if got := compactJSON(t, got[1].Detail); got != `{"chunks":3}` {
		t.Errorf("parse_finished detail = %s, want {\"chunks\":3}", got)
	}

	after, err := events.ListIngestEventsAfter(ctx, doc.ID, second.Seq)
	if err != nil {
		t.Fatalf("list after the second event: %v", err)
	}
	if len(after) != 1 || after[0].Seq != third.Seq || after[0].Type != types.IngestEventCompleted {
		t.Errorf("events after seq %d = %+v, want only completed at %d", second.Seq, after, third.Seq)
	}
}

// TestIngestEventsTakeTheirSeqsInCommitOrder is the regression test for the
// global bigserial cursor bug: tx A could take seq 10 and tx B seq 11, B could
// commit first, and a reader that took 11 as its cursor would never see 10
// when A committed. A Document's events must take their seq under a lock the
// next insert waits for, so seq order is commit order per Document.
func TestIngestEventsTakeTheirSeqsInCommitOrder(t *testing.T) {
	ctx := context.Background()
	s := db.NewStore(dbtest.Migrated(t))
	owner, err := s.Querier(ctx).CreatePrincipal(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	doc := document(t, s, owner.ID)
	events := NewIngestEventRepository(s)

	firstInserted := make(chan struct{})
	allowFirstCommit := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- s.ExecTx(ctx, func(ctx context.Context) error {
			if _, err := events.InsertIngestEvent(ctx, &types.IngestEvent{
				DocumentID: doc.ID,
				Type:       types.IngestEventParseStarted,
				Detail:     json.RawMessage(`{"attempt":1}`),
			}); err != nil {
				return err
			}
			close(firstInserted)
			<-allowFirstCommit
			return nil
		})
	}()
	select {
	case <-firstInserted:
	case <-time.After(5 * time.Second):
		t.Fatal("the first event was not inserted")
	}

	// The second insert must wait for the first transaction to commit.
	// Committing it first would let a reader take its seq as the cursor and
	// miss the lower seq of the event that is still open.
	secondInserted := make(chan struct{})
	secondErr := make(chan error, 1)
	go func() {
		_, err := events.InsertIngestEvent(ctx, &types.IngestEvent{
			DocumentID: doc.ID,
			Type:       types.IngestEventParseFinished,
			Detail:     json.RawMessage(`{"chunks":1}`),
		})
		if err == nil {
			close(secondInserted)
		}
		secondErr <- err
	}()
	select {
	case <-secondInserted:
		close(allowFirstCommit)
		firstErr := <-firstDone
		t.Fatalf("the second event committed before the first transaction (%v); a reader could take its seq and miss the first event", firstErr)
	case <-time.After(500 * time.Millisecond):
		// Waiting on the Document row lock, as it must.
	}

	close(allowFirstCommit)
	if err := <-firstDone; err != nil {
		t.Fatalf("first transaction: %v", err)
	}
	select {
	case err := <-secondErr:
		if err != nil {
			t.Fatalf("second insert: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second insert did not finish after the first transaction committed")
	}

	got, err := events.ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("events = %+v, want seq 1 then 2 in commit order", got)
	}
	if got[0].Type != types.IngestEventParseStarted || got[1].Type != types.IngestEventParseFinished {
		t.Errorf("event order = %s, %s; want parse_started then parse_finished", got[0].Type, got[1].Type)
	}
}

// A Document deleted before its event is inserted has no counter row to
// advance, so the insert matches no Document and is the repository's
// not-found error, not a foreign-key failure.
func TestInsertingAnEventForAMissingDocumentIsNotFound(t *testing.T) {
	s, _ := seeded(t)
	_, err := NewIngestEventRepository(s).InsertIngestEvent(context.Background(), &types.IngestEvent{
		DocumentID: uuid.New(),
		Type:       types.IngestEventParseStarted,
		Detail:     json.RawMessage(`{"attempt":1}`),
	})
	if !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("InsertIngestEvent(unknown Document) = %v, want ErrDocumentNotFound", err)
	}
}

func TestADeletedDocumentTakesItsIngestEventsWithIt(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	s := db.NewStore(pool)
	owner, err := s.Querier(ctx).CreatePrincipal(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	doc := document(t, s, owner.ID)
	events := NewIngestEventRepository(s)
	for _, eventType := range []string{types.IngestEventParseStarted, types.IngestEventCompleted} {
		if _, err := events.InsertIngestEvent(ctx, &types.IngestEvent{
			DocumentID: doc.ID,
			Type:       eventType,
			Detail:     json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("insert %s: %v", eventType, err)
		}
	}

	if err := NewDocumentRepository(s).DeleteDocument(ctx, doc.ID); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}

	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM ingest_events WHERE document_id = $1", doc.ID).Scan(&rows); err != nil {
		t.Fatalf("count ingest_events: %v", err)
	}
	if rows != 0 {
		t.Errorf("ingest_events holds %d rows for the deleted Document, want 0 by cascade", rows)
	}
	listed, err := events.ListIngestEventsAfter(ctx, doc.ID, 0)
	if err != nil || len(listed) != 0 {
		t.Errorf("ListIngestEventsAfter(deleted Document) = %+v, %v; want an empty list and no error", listed, err)
	}
}

// A Document with no events is not an error; the handler polls it while the
// workflow has not written anything yet.
func TestListingEventsOfADocumentWithNoneIsEmpty(t *testing.T) {
	s, ids := seeded(t)
	doc := document(t, s, ids["alice"])
	got, err := NewIngestEventRepository(s).ListIngestEventsAfter(context.Background(), doc.ID, 0)
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("ListIngestEventsAfter = %#v, %v; want an empty, non-nil list", got, err)
	}
	if _, err := NewIngestEventRepository(s).ListIngestEventsAfter(context.Background(), uuid.New(), 0); err != nil {
		t.Errorf("ListIngestEventsAfter(unknown Document) = %v, want no error", err)
	}
}
