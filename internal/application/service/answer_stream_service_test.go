package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	apperrors "github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/logger"
	"github.com/phongnd2802/trelis/internal/shutdown"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// answerToken is one answer stream chunk carrying text.
func answerToken(text string) types.StreamResponse {
	return types.StreamResponse{ResponseType: types.ResponseTypeAnswer, Content: text}
}

// answerClose is the closing chunk of a healthy chat stream.
func answerClose(usage *types.TokenUsage, finishReason string) types.StreamResponse {
	return types.StreamResponse{ResponseType: types.ResponseTypeAnswer, Done: true, Usage: usage, FinishReason: finishReason}
}

// answerErrorChunk is the closing chunk of a broken chat stream; its content
// is the provider's error, which must never reach the client.
func answerErrorChunk(text string) types.StreamResponse {
	return types.StreamResponse{ResponseType: types.ResponseTypeError, Content: text, Done: true}
}

// collectAnswerEvents drains a stream through the service until it ends.
func collectAnswerEvents(t *testing.T, ctx context.Context, svc interfaces.AnswerService, answerID uuid.UUID) []types.AnswerEvent {
	t.Helper()
	var events []types.AnswerEvent
	after := int64(1) // started
	deadline := time.After(5 * time.Second)
	for {
		subscription, err := svc.AnswerEvents(ctx, answerID, after)
		if err != nil {
			t.Fatalf("AnswerEvents after %d: %v", after, err)
		}
		for _, event := range subscription.Events {
			if event.Seq != after+1 {
				t.Fatalf("event seq %d follows %d, want contiguous sequence numbers", event.Seq, after)
			}
			after = event.Seq
		}
		events = append(events, subscription.Events...)
		if subscription.Done {
			return events
		}
		select {
		case <-subscription.Wait:
		case <-deadline:
			t.Fatalf("stream did not end; events so far: %+v", events)
		}
	}
}

func TestStreamedAnswerEmitsTokensThenCitationsThenDone(t *testing.T) {
	results := answerResults()
	usage := types.TokenUsage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}
	raw := "Alpha [1] and gamma [3]. Beta [1][3]."
	gen := &fakeGenerator{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeThinking, Content: "reasoning that must not stream"},
		{ResponseType: types.ResponseTypeThinking, Done: true},
		answerToken("Alpha "),
		answerToken("[1] and "),
		answerToken("gamma [3]. Beta [1][3]."),
		answerClose(&usage, "stop"),
	}}
	svc := NewAnswerService(&stubAnswerRetrieval{result: &types.RetrievalResult{Results: results}}, gen, nil, &fakeReadableDocuments{})
	ctx := callerContext(uuid.New())

	answerID, err := svc.StartAnswerStream(ctx, answerRequest("who won?", true))
	if err != nil {
		t.Fatalf("StartAnswerStream: %v", err)
	}
	events := collectAnswerEvents(t, ctx, svc, answerID)

	var tokens []string
	var citations []types.AnswerCitationEvent
	var done *types.AnswerDoneEvent
	for _, event := range events {
		switch data := event.Data.(type) {
		case types.AnswerTokenEvent:
			if event.Type != types.AnswerEventToken {
				t.Fatalf("event %q carries a token payload", event.Type)
			}
			tokens = append(tokens, data.Text)
		case types.AnswerCitationEvent:
			if event.Type != types.AnswerEventCitation {
				t.Fatalf("event %q carries a citation payload", event.Type)
			}
			citations = append(citations, data)
		case types.AnswerDoneEvent:
			if event.Type != types.AnswerEventDone {
				t.Fatalf("event %q carries a done payload", event.Type)
			}
			done = &data
		}
	}
	if got := strings.Join(tokens, ""); got != raw {
		t.Errorf("tokens concatenate to %q, want the raw reply %q", got, raw)
	}
	if strings.Contains(strings.Join(tokens, ""), "reasoning") {
		t.Error("the token events carry thinking text, want only answer text")
	}
	if len(citations) != 2 || citations[0] != (types.AnswerCitationEvent{
		Marker: 1, ChunkID: results[0].ChunkID, DocumentID: results[0].DocumentID,
	}) || citations[1] != (types.AnswerCitationEvent{
		Marker: 3, ChunkID: results[2].ChunkID, DocumentID: results[2].DocumentID,
	}) {
		t.Errorf("citations = %+v, want sources 1 and 3 in first-appearance order", citations)
	}
	if done == nil {
		t.Fatal("no done event")
	}
	if done.Answer != raw {
		t.Errorf("done answer = %q, want the cleaned text %q", done.Answer, raw)
	}
	if done.FinishReason != "stop" {
		t.Errorf("done finish_reason = %q, want stop", done.FinishReason)
	}
	if done.Debug == nil {
		t.Fatal("done debug = nil, want the retrieved Chunks, trace and usage")
	}
	if done.Debug.Usage != usage {
		t.Errorf("done debug usage = %+v, want the chat call's usage %+v", done.Debug.Usage, usage)
	}
	if len(done.Debug.Trace) != 1 || done.Debug.Trace[0].Step != types.TraceStepGenerate ||
		done.Debug.Trace[0].Model != gen.model {
		t.Errorf("done trace = %+v, want the generate step", done.Debug.Trace)
	}
	if events[len(events)-1].Type != types.AnswerEventDone {
		t.Errorf("last event = %q, want done", events[len(events)-1].Type)
	}
}

func TestStreamedAnswerCitesExactlyLikeTheJSONAnswer(t *testing.T) {
	results := answerResults()
	raw := "Alpha [1], nowhere [9], both [1, 3]."
	usage := types.TokenUsage{PromptTokens: 5, CompletionTokens: 6, TotalTokens: 11}
	gen := &fakeGenerator{
		reply:  &types.ChatResponse{Content: raw, FinishReason: "stop", Usage: usage},
		chunks: []types.StreamResponse{answerToken(raw), answerClose(&usage, "stop")},
	}
	svc := NewAnswerService(&stubAnswerRetrieval{result: &types.RetrievalResult{Results: results}}, gen, nil, &fakeReadableDocuments{})
	ctx := callerContext(uuid.New())

	jsonAnswer, err := svc.Answer(ctx, answerRequest("who won?", false))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	answerID, err := svc.StartAnswerStream(ctx, answerRequest("who won?", false))
	if err != nil {
		t.Fatalf("StartAnswerStream: %v", err)
	}
	events := collectAnswerEvents(t, ctx, svc, answerID)

	var citations []types.AnswerCitationEvent
	var done types.AnswerDoneEvent
	for _, event := range events {
		switch data := event.Data.(type) {
		case types.AnswerCitationEvent:
			citations = append(citations, data)
		case types.AnswerDoneEvent:
			done = data
		}
	}
	if len(citations) != len(jsonAnswer.Citations) {
		t.Fatalf("streamed %d citations, JSON answered %d", len(citations), len(jsonAnswer.Citations))
	}
	for i, citation := range citations {
		want := jsonAnswer.Citations[i]
		if citation.Marker != want.Marker || citation.ChunkID != want.ChunkID || citation.DocumentID != want.DocumentID {
			t.Errorf("citation %d = %+v, want %+v from the JSON answer", i, citation, want)
		}
	}
	if done.Answer != jsonAnswer.Answer {
		t.Errorf("done answer = %q, want the JSON answer %q", done.Answer, jsonAnswer.Answer)
	}
	if done.FinishReason != jsonAnswer.FinishReason {
		t.Errorf("done finish_reason = %q, want %q", done.FinishReason, jsonAnswer.FinishReason)
	}
	if strings.Contains(done.Answer, "[9]") {
		t.Errorf("done answer = %q, want the out-of-range marker removed", done.Answer)
	}
}

func TestAStreamErrorChunkEndsWithAGenericError(t *testing.T) {
	gen := &fakeGenerator{chunks: []types.StreamResponse{
		answerToken("Partial "),
		answerErrorChunk("the provider said secret document text"),
	}}
	svc := NewAnswerService(&stubAnswerRetrieval{result: &types.RetrievalResult{Results: answerResults()}}, gen, nil, &fakeReadableDocuments{})
	ctx := callerContext(uuid.New())

	answerID, err := svc.StartAnswerStream(ctx, answerRequest("who won?", false))
	if err != nil {
		t.Fatalf("StartAnswerStream: %v", err)
	}
	events := collectAnswerEvents(t, ctx, svc, answerID)

	last := events[len(events)-1]
	failure, ok := last.Data.(types.AnswerErrorEvent)
	if !ok || last.Type != types.AnswerEventError {
		t.Fatalf("last event = %+v, want the error event", last)
	}
	if failure.Message != answerStreamErrorMessage {
		t.Errorf("error message = %q, want the generic %q", failure.Message, answerStreamErrorMessage)
	}
	for _, event := range events {
		if event.Type == types.AnswerEventDone {
			t.Error("a failed stream also carried a done event, want only the error")
		}
	}
	if strings.Contains(failure.Message, "secret document text") {
		t.Errorf("error message = %q, want the provider's text left out", failure.Message)
	}
}

func TestAStreamErrorChunkLogsNoProviderText(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	ctx := logger.WithContext(callerContext(uuid.New()), zap.New(core))
	const providerText = "the provider said secret document text"
	gen := &fakeGenerator{chunks: []types.StreamResponse{answerErrorChunk(providerText)}}
	svc := NewAnswerService(&stubAnswerRetrieval{result: &types.RetrievalResult{Results: answerResults()}}, gen, nil, &fakeReadableDocuments{})

	answerID, err := svc.StartAnswerStream(ctx, answerRequest("who won?", false))
	if err != nil {
		t.Fatalf("StartAnswerStream: %v", err)
	}
	collectAnswerEvents(t, ctx, svc, answerID)

	for _, entry := range logs.All() {
		if strings.Contains(entry.Message, providerText) {
			t.Errorf("log message %q carries the provider's text", entry.Message)
		}
		for name, value := range entry.ContextMap() {
			if strings.Contains(fmt.Sprint(value), providerText) {
				t.Errorf("log field %q = %v carries the provider's text", name, value)
			}
		}
	}
	entries := logs.FilterMessage("answer stream generation failed").All()
	if len(entries) != 1 {
		t.Fatalf("logged %d generation failures, want one: %v", len(entries), logs.All())
	}
	fields := entries[0].ContextMap()
	if fields["role"] != "chat" {
		t.Errorf("failure role = %v, want chat", fields["role"])
	}
	if fields["answer"] != answerID.String() {
		t.Errorf("failure answer = %v, want %s", fields["answer"], answerID)
	}
	if got := fmt.Sprint(fields["error"]); got != "generate streamed pipeline answer: the chat stream ended with an error" {
		t.Errorf("failure cause = %q, want the fixed stream-error cause", got)
	}
}

func TestAStreamWithoutSourcesAnswersWithoutCallingTheChatRole(t *testing.T) {
	gen := &forbiddenAnswerGenerator{t}
	svc := NewAnswerService(
		&stubAnswerRetrieval{result: &types.RetrievalResult{Results: []types.RetrievedChunk{}}},
		gen, nil, &fakeReadableDocuments{})

	ctx := callerContext(uuid.New())

	answerID, err := svc.StartAnswerStream(ctx, &types.AnswerRequest{
		RetrievalRequest: types.RetrievalRequest{Query: "who won?"},
	})
	if err != nil {
		t.Fatalf("StartAnswerStream: %v", err)
	}
	events := collectAnswerEvents(t, ctx, svc, answerID)

	var tokens, done int
	for _, event := range events {
		switch event.Type {
		case types.AnswerEventToken:
			tokens++
			if token := event.Data.(types.AnswerTokenEvent); token.Text != noSourcesAnswer {
				t.Errorf("token = %q, want the fixed no-source sentence", token.Text)
			}
		case types.AnswerEventCitation:
			t.Error("a no-source answer emitted a citation")
		case types.AnswerEventDone:
			done++
			if answer := event.Data.(types.AnswerDoneEvent); answer.Answer != noSourcesAnswer ||
				answer.FinishReason != "" {
				t.Errorf("done = %+v, want the fixed sentence and no finish reason", answer)
			}
		}
	}
	if tokens != 1 || done != 1 {
		t.Errorf("streamed %d tokens and %d done events, want one of each", tokens, done)
	}
}

func TestAStreamKeepsGeneratingAfterTheRequestIsCancelled(t *testing.T) {
	release := make(chan struct{})
	contextErr := make(chan error, 1)
	gen := &fakeGenerator{stream: func(ctx context.Context) (<-chan types.StreamResponse, error) {
		ch := make(chan types.StreamResponse)
		go func() {
			defer close(ch)
			<-release
			contextErr <- ctx.Err()
			if ctx.Err() != nil {
				return
			}
			ch <- answerToken("Alpha ")
			ch <- answerClose(nil, "stop")
		}()
		return ch, nil
	}}
	svc := NewAnswerService(&stubAnswerRetrieval{result: &types.RetrievalResult{Results: answerResults()}}, gen, nil, &fakeReadableDocuments{})
	principalID := uuid.New()
	requestCtx, cancel := context.WithCancel(callerContext(principalID))

	answerID, err := svc.StartAnswerStream(requestCtx, answerRequest("who won?", false))
	if err != nil {
		t.Fatalf("StartAnswerStream: %v", err)
	}
	cancel() // the client dropped
	close(release)

	events := collectAnswerEvents(t, callerContext(principalID), svc, answerID)
	if err := <-contextErr; err != nil {
		t.Errorf("generation context = %v after the request was cancelled, want it still alive", err)
	}
	last := events[len(events)-1]
	done, ok := last.Data.(types.AnswerDoneEvent)
	if !ok || done.Answer != "Alpha " {
		t.Fatalf("last event = %+v, want done with the full answer", last)
	}
}

func TestTheGenerationTimeoutEndsTheStream(t *testing.T) {
	gen := &fakeGenerator{stream: func(ctx context.Context) (<-chan types.StreamResponse, error) {
		ch := make(chan types.StreamResponse)
		go func() {
			defer close(ch)
			<-ctx.Done()
		}()
		return ch, nil
	}}
	svc := &answerService{
		retrieval:         &stubAnswerRetrieval{result: &types.RetrievalResult{Results: answerResults()}},
		generator:         gen,
		streams:           newAnswerStreamHub(time.Hour, answerStreamMaxEvents),
		generationTimeout: 20 * time.Millisecond,
	}
	ctx := callerContext(uuid.New())

	answerID, err := svc.StartAnswerStream(ctx, answerRequest("who won?", false))
	if err != nil {
		t.Fatalf("StartAnswerStream: %v", err)
	}
	events := collectAnswerEvents(t, ctx, svc, answerID)
	if last := events[len(events)-1]; last.Type != types.AnswerEventError {
		t.Errorf("last event = %q, want the error event after the timeout", last.Type)
	}
}

func TestShutdownCancelsTheGeneration(t *testing.T) {
	signal := make(chan struct{})
	gen := &fakeGenerator{stream: func(ctx context.Context) (<-chan types.StreamResponse, error) {
		ch := make(chan types.StreamResponse)
		go func() {
			defer close(ch)
			<-ctx.Done()
		}()
		return ch, nil
	}}
	svc := NewAnswerService(&stubAnswerRetrieval{result: &types.RetrievalResult{Results: answerResults()}}, gen, nil, &fakeReadableDocuments{})
	principalID := uuid.New()
	ctx := shutdown.WithSignal(callerContext(principalID), signal)

	answerID, err := svc.StartAnswerStream(ctx, answerRequest("who won?", false))
	if err != nil {
		t.Fatalf("StartAnswerStream: %v", err)
	}
	close(signal)

	events := collectAnswerEvents(t, callerContext(principalID), svc, answerID)
	if last := events[len(events)-1]; last.Type != types.AnswerEventError {
		t.Errorf("last event = %q, want the error event after shutdown", last.Type)
	}
}

func TestShutdownCancelsAGenerationWaitingForHeaders(t *testing.T) {
	signal := make(chan struct{})
	gen := &fakeGenerator{stream: func(ctx context.Context) (<-chan types.StreamResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	svc := NewAnswerService(&stubAnswerRetrieval{result: &types.RetrievalResult{Results: answerResults()}}, gen, nil, &fakeReadableDocuments{})
	principalID := uuid.New()
	ctx := shutdown.WithSignal(callerContext(principalID), signal)

	started := make(chan error, 1)
	go func() {
		_, err := svc.StartAnswerStream(ctx, answerRequest("who won?", false))
		started <- err
	}()
	close(signal) // the server begins shutting down while the call waits

	select {
	case err := <-started:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("StartAnswerStream = %v, want the shutdown to cancel the open call", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StartAnswerStream kept waiting after shutdown")
	}
}

func TestStartAnswerStreamReturnsAProviderRejection(t *testing.T) {
	hub := newAnswerStreamHub(time.Hour, 100)
	svc := &answerService{
		retrieval:         &stubAnswerRetrieval{result: &types.RetrievalResult{Results: answerResults()}},
		generator:         &fakeGenerator{streamErr: apperrors.NewModelProviderError("chat", http.StatusUnauthorized, errors.New("the provider said secret"))},
		streams:           hub,
		generationTimeout: time.Minute,
	}
	_, err := svc.StartAnswerStream(callerContext(uuid.New()), answerRequest("who won?", false))
	appErr, ok := apperrors.IsAppError(err)
	if !ok || appErr.Code != apperrors.ErrModelProvider || appErr.HTTPCode != http.StatusBadGateway {
		t.Fatalf("StartAnswerStream = %v, want the provider-rejection 502", err)
	}
	if strings.Contains(appErr.Message, "secret") {
		t.Errorf("error message = %q, want the provider's text left out", appErr.Message)
	}
	if len(hub.streams) != 0 {
		t.Errorf("hub holds %d streams, want none when the model call was rejected", len(hub.streams))
	}
}

func TestCloseWaitsForGeneration(t *testing.T) {
	release := make(chan struct{})
	gen := &fakeGenerator{stream: func(ctx context.Context) (<-chan types.StreamResponse, error) {
		ch := make(chan types.StreamResponse)
		go func() {
			defer close(ch)
			select {
			case <-release:
				ch <- answerToken("Alpha ")
				ch <- answerClose(nil, "stop")
			case <-ctx.Done():
			}
		}()
		return ch, nil
	}}
	svc := NewAnswerService(&stubAnswerRetrieval{result: &types.RetrievalResult{Results: answerResults()}}, gen, nil, &fakeReadableDocuments{})
	ctx := callerContext(uuid.New())
	if _, err := svc.StartAnswerStream(ctx, answerRequest("who won?", false)); err != nil {
		t.Fatalf("StartAnswerStream: %v", err)
	}

	shortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := svc.Close(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close while generating = %v, want the context deadline", err)
	}
	close(release)
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWait()
	if err := svc.Close(waitCtx); err != nil {
		t.Fatalf("Close after generation = %v, want nil", err)
	}
}

func TestDoneCarriesDebugOnlyWhenAsked(t *testing.T) {
	results := answerResults()
	usage := types.TokenUsage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}
	for name, tc := range map[string]struct {
		debug bool
		want  bool
	}{
		"debug asked":   {debug: true, want: true},
		"debug omitted": {debug: false, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			gen := &fakeGenerator{
				model:  "test-chat",
				chunks: []types.StreamResponse{answerToken("Alpha [1]."), answerClose(&usage, "stop")},
			}
			svc := NewAnswerService(&stubAnswerRetrieval{result: &types.RetrievalResult{Results: results}}, gen, nil, &fakeReadableDocuments{})
			ctx := callerContext(uuid.New())
			answerID, err := svc.StartAnswerStream(ctx, answerRequest("who won?", tc.debug))
			if err != nil {
				t.Fatalf("StartAnswerStream: %v", err)
			}
			events := collectAnswerEvents(t, ctx, svc, answerID)
			done := events[len(events)-1].Data.(types.AnswerDoneEvent)
			if (done.Debug != nil) != tc.want {
				t.Fatalf("done debug = %+v, want present %v", done.Debug, tc.want)
			}
			if !tc.want {
				return
			}
			if len(done.Debug.Results) != len(results) || done.Debug.Usage != usage {
				t.Errorf("done debug = %+v, want the retrieved Chunks and the call's usage", done.Debug)
			}
			if len(done.Debug.Trace) != 1 || done.Debug.Trace[0].Step != types.TraceStepGenerate ||
				done.Debug.Trace[0].Model != "test-chat" {
				t.Errorf("done trace = %+v, want the generate step naming the chat model", done.Debug.Trace)
			}
		})
	}
}

func TestStartAnswerStreamReturnsPreGenerationErrors(t *testing.T) {
	retrieval := &stubAnswerRetrieval{result: &types.RetrievalResult{Results: answerResults()}}
	svc := NewAnswerService(retrieval, nil, nil, &fakeReadableDocuments{})

	if _, err := svc.StartAnswerStream(callerContext(uuid.New()), answerRequest("who won?", false)); err == nil {
		t.Error("StartAnswerStream without models.chat = nil, want a 503")
	}
	if retrieval.calls != 0 {
		t.Errorf("retrieval called %d times without a chat model, want 0", retrieval.calls)
	}
}

func TestAStreamedAnswerRecordsTheTurnAndSumsTheRewriteUsage(t *testing.T) {
	conversationID, principalID := uuid.New(), uuid.New()
	rewriteUsage := types.TokenUsage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14}
	generateUsage := types.TokenUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}
	conversations := &fakeConversations{owned: true}
	rewritten := "standalone"
	retrieval := &stubAnswerRetrieval{result: &types.RetrievalResult{
		Results:        answerResults(),
		Query:          rewritten,
		RewrittenQuery: &rewritten,
		RewriteUsage:   rewriteUsage,
		Trace: []types.TraceStep{{
			Step: types.TraceStepRewriteQuery, Technique: types.RetrievalTechniqueQueryRewrite,
			RewrittenQuery: rewritten, Usage: &rewriteUsage,
		}},
	}}
	gen := &fakeGenerator{chunks: []types.StreamResponse{
		answerToken("Alpha [1]."),
		answerClose(&generateUsage, "stop"),
	}}
	svc := NewAnswerService(retrieval, gen, conversations, &fakeReadableDocuments{})
	ctx := callerContext(principalID)

	answerID, err := svc.StartAnswerStream(ctx, conversationAnswerRequest("follow-up", conversationID, true))
	if err != nil {
		t.Fatalf("StartAnswerStream: %v", err)
	}
	events := collectAnswerEvents(t, ctx, svc, answerID)

	last := events[len(events)-1]
	done, ok := last.Data.(types.AnswerDoneEvent)
	if !ok || last.Type != types.AnswerEventDone {
		t.Fatalf("last event = %+v, want done", last)
	}
	wantUsage := types.TokenUsage{PromptTokens: 13, CompletionTokens: 6, TotalTokens: 19}
	if done.Debug == nil || done.Debug.Usage != wantUsage {
		t.Errorf("done debug = %+v, want the rewrite and generate usage summed to %+v", done.Debug, wantUsage)
	}
	if conversations.inserts != 1 {
		t.Fatalf("recorded %d turns, want exactly one", conversations.inserts)
	}
	turn := conversations.inserted[0]
	if turn.ConversationID != conversationID || turn.Question != "follow-up" || turn.Answer != "Alpha [1]." {
		t.Errorf("recorded turn = %+v, want the raw question and cleaned answer", turn)
	}
	if turn.RewrittenQuery == nil || *turn.RewrittenQuery != "standalone" {
		t.Errorf("recorded rewritten query = %v, want standalone", turn.RewrittenQuery)
	}
	if conversations.insertedAs[0] != principalID {
		t.Errorf("turn recorded as %s, want the caller %s", conversations.insertedAs[0], principalID)
	}
}

func TestAStreamedRecordFailureEndsWithTheErrorEvent(t *testing.T) {
	cause := errors.New("postgres down")
	conversations := &fakeConversations{owned: true, insertErr: cause}
	gen := &fakeGenerator{chunks: []types.StreamResponse{
		answerToken("Alpha [1]."),
		answerClose(nil, "stop"),
	}}
	svc := NewAnswerService(&stubAnswerRetrieval{result: &types.RetrievalResult{Results: answerResults()}}, gen, conversations, &fakeReadableDocuments{})

	ctx := callerContext(uuid.New())

	answerID, err := svc.StartAnswerStream(ctx, conversationAnswerRequest("who won?", uuid.New(), false))
	if err != nil {
		t.Fatalf("StartAnswerStream: %v", err)
	}
	events := collectAnswerEvents(t, ctx, svc, answerID)

	last := events[len(events)-1]
	failure, ok := last.Data.(types.AnswerErrorEvent)
	if !ok || last.Type != types.AnswerEventError {
		t.Fatalf("last event = %+v, want the error event when the turn cannot be recorded", last)
	}
	if failure.Message != answerStreamErrorMessage || strings.Contains(failure.Message, "postgres") {
		t.Errorf("error message = %q, want the generic client message", failure.Message)
	}
	for _, event := range events {
		if event.Type == types.AnswerEventDone {
			t.Error("a failed record also emitted done, want only the error")
		}
	}
	if conversations.inserts != 1 {
		t.Errorf("record attempts = %d, want the one that failed", conversations.inserts)
	}
}

func TestAFailedStreamedGenerationRecordsNoTurn(t *testing.T) {
	conversations := &fakeConversations{owned: true}
	gen := &fakeGenerator{chunks: []types.StreamResponse{
		answerToken("Partial "),
		answerErrorChunk("the provider said secret"),
	}}
	svc := NewAnswerService(&stubAnswerRetrieval{result: &types.RetrievalResult{Results: answerResults()}}, gen, conversations, &fakeReadableDocuments{})

	ctx := callerContext(uuid.New())

	answerID, err := svc.StartAnswerStream(ctx, conversationAnswerRequest("who won?", uuid.New(), false))
	if err != nil {
		t.Fatalf("StartAnswerStream: %v", err)
	}
	collectAnswerEvents(t, ctx, svc, answerID)

	if conversations.inserts != 0 || conversations.getCalls != 1 {
		t.Errorf("conversation calls = %d gets, %d records; want the ownership check and no record",
			conversations.getCalls, conversations.inserts)
	}
}

func TestANoSourcesStreamRecordsTheTurn(t *testing.T) {
	conversationID := uuid.New()
	rewriteUsage := types.TokenUsage{PromptTokens: 6, CompletionTokens: 2, TotalTokens: 8}
	conversations := &fakeConversations{owned: true}
	rewritten := "standalone"
	retrieval := &stubAnswerRetrieval{result: &types.RetrievalResult{
		Results:        []types.RetrievedChunk{},
		Query:          rewritten,
		RewrittenQuery: &rewritten,
		RewriteUsage:   rewriteUsage,
		Trace: []types.TraceStep{{
			Step: types.TraceStepRewriteQuery, Technique: types.RetrievalTechniqueQueryRewrite,
			RewrittenQuery: rewritten, Usage: &rewriteUsage,
		}},
	}}
	svc := NewAnswerService(retrieval, &forbiddenAnswerGenerator{t}, conversations, &fakeReadableDocuments{})
	ctx := callerContext(uuid.New())

	answerID, err := svc.StartAnswerStream(ctx, conversationAnswerRequest("follow-up", conversationID, true))
	if err != nil {
		t.Fatalf("StartAnswerStream: %v", err)
	}
	events := collectAnswerEvents(t, ctx, svc, answerID)
	done := events[len(events)-1].Data.(types.AnswerDoneEvent)
	if done.Answer != noSourcesAnswer {
		t.Errorf("done answer = %q, want the fixed no-source sentence", done.Answer)
	}
	if done.Debug == nil || done.Debug.Usage != rewriteUsage {
		t.Errorf("done debug = %+v, want the rewrite call's usage", done.Debug)
	}
	if conversations.inserts != 1 {
		t.Fatalf("recorded %d turns, want one even for a no-source answer", conversations.inserts)
	}
	turn := conversations.inserted[0]
	if turn.Question != "follow-up" || turn.Answer != noSourcesAnswer ||
		turn.RewrittenQuery == nil || *turn.RewrittenQuery != "standalone" {
		t.Errorf("recorded turn = %+v, want the raw question, fixed answer and rewritten query", turn)
	}
}
