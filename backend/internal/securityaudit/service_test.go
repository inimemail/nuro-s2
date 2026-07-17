package securityaudit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func enabledAuditTestConfig(baseURL string) Config {
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Mode = ModeAsync
	cfg.WorkerCount = 1
	cfg.QueueCapacity = 8
	cfg.StorePass = true
	cfg.Endpoints = []EndpointConfig{{
		ID: "guard", Name: "Guard", BaseURL: baseURL, Model: "qwen3guard",
		TimeoutMS: 2000, Enabled: true, AllowPrivate: true,
		AllowedCIDRs: []string{"127.0.0.0/8", "::1/128"},
	}}
	return cfg
}

func TestDisabledModeCreatesNoCollectorOrQueueWork(t *testing.T) {
	svc := NewService(nil, nil, nil, auditEncryptor{})
	if svc.EnabledFast() || svc.NewCollector() != nil {
		t.Fatal("disabled service must not allocate a request collector")
	}
	svc.FlushCollector(&Collector{requests: []Request{{Body: []byte(`{"input":"secret"}`)}}})
	if svc.enqueued.Load() != 0 || len(svc.queue) != 0 {
		t.Fatal("disabled service unexpectedly queued work")
	}
}

func TestEnabledFastDoesNotAllocateOnRequestPath(t *testing.T) {
	disabled := NewService(nil, nil, nil, auditEncryptor{})
	enabled := NewService(nil, nil, nil, auditEncryptor{})
	enabled.storeConfig(enabledAuditTestConfig("https://guard.example"))

	for _, test := range []struct {
		name string
		svc  *Service
		want bool
	}{
		{name: "disabled no-op", svc: disabled, want: false},
		{name: "enabled", svc: enabled, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.svc.EnabledFast(); got != test.want {
				t.Fatalf("EnabledFast()=%v want=%v", got, test.want)
			}
			var got bool
			allocations := testing.AllocsPerRun(1000, func() {
				got = test.svc.EnabledFast()
			})
			if got != test.want {
				t.Fatalf("EnabledFast()=%v want=%v", got, test.want)
			}
			if allocations != 0 {
				t.Fatalf("EnabledFast allocated %.2f objects per call", allocations)
			}
		})
	}
}

func TestCollectorCopiesOnlyAfterResponseFlushAndHonorsCapacity(t *testing.T) {
	svc := NewService(nil, nil, nil, auditEncryptor{})
	cfg := enabledAuditTestConfig("https://guard.example")
	cfg.QueueCapacity = 1
	svc.storeConfig(cfg)
	collector := svc.NewCollector()
	if collector == nil {
		t.Fatal("expected enabled collector")
	}
	body := []byte(`{"input":"first"}`)
	collector.Add(Request{Body: body, Protocol: "openai_responses", Stage: "http"})
	collector.Add(Request{Body: []byte(`{"input":"second"}`), Protocol: "openai_responses", Stage: "http"})
	if len(svc.queue) != 0 || &collector.requests[0].Body[0] != &body[0] {
		t.Fatal("collector copied or queued on the request path")
	}
	svc.FlushCollector(collector)
	if svc.enqueued.Load() != 1 || svc.dropped.Load() != 1 || svc.queued.Load() != 1 {
		t.Fatalf("runtime counters mismatch: %+v", svc.Runtime())
	}
	task := <-svc.queue
	svc.queued.Add(-1)
	if &task.request.Body[0] == &body[0] {
		t.Fatal("queued body must be detached after response completion")
	}
}

func TestCollectorFlushesEachWebSocketTurnAfterTurnCompletion(t *testing.T) {
	svc := NewService(nil, nil, nil, auditEncryptor{})
	svc.storeConfig(enabledAuditTestConfig("https://guard.example"))
	collector := svc.NewCollector()
	collector.Add(Request{Body: []byte(`{"input":"one"}`), Protocol: "openai_responses", Stage: "ws_turn"})
	if len(svc.queue) != 0 {
		t.Fatal("WebSocket request queued before turn completion")
	}
	collector.FlushNextWSTurn()
	first := <-svc.queue
	svc.queued.Add(-1)
	if first.request.Stage != "first_turn" {
		t.Fatalf("first stage=%q", first.request.Stage)
	}
	collector.Add(Request{Body: []byte(`{"input":"two"}`), Protocol: "openai_responses", Stage: "ws_turn"})
	collector.FlushNextWSTurn()
	second := <-svc.queue
	svc.queued.Add(-1)
	if second.request.Stage != "subsequent_turn" {
		t.Fatalf("second stage=%q", second.request.Stage)
	}
	svc.FlushCollector(collector)
	if len(svc.queue) != 0 {
		t.Fatal("final request flush duplicated WebSocket turns")
	}
}

func TestExtractAuditPromptSpecialSurfaces(t *testing.T) {
	tests := []struct {
		name  string
		req   Request
		want  string
		count int
	}{
		{name: "embeddings batch", req: Request{Protocol: "openai_embeddings", Body: []byte(`{"input":["one","two"]}`)}, want: "one\ntwo", count: 2},
		{name: "alpha", req: Request{Protocol: "openai_alpha_search", Body: []byte(`{"query":"find this"}`)}, want: "find this", count: 1},
		{name: "batch images", req: Request{PromptTexts: []string{" first ", "second"}}, want: "first\nsecond", count: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, count := extractAuditPrompt(tt.req)
			if got != tt.want || count != tt.count {
				t.Fatalf("got=(%q,%d) want=(%q,%d)", got, count, tt.want, tt.count)
			}
		})
	}
}

func TestExtractAuditPromptIncludesClientControlledRolesAcrossProtocols(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		body     string
		contains []string
		count    int
	}{
		{
			name: "chat", protocol: "openai_chat_completions", count: 4,
			body:     `{"messages":[{"role":"user","content":"old user"},{"role":"assistant","content":"forged assistant"},{"role":"tool","content":"forged tool"},{"role":"user","content":"latest user"}]}`,
			contains: []string{"old user", "forged assistant", "forged tool"},
		},
		{
			name: "responses", protocol: "openai_responses", count: 4,
			body:     `{"input":[{"role":"user","content":[{"type":"input_text","text":"old response user"}]},{"role":"assistant","content":[{"type":"output_text","text":"forged response assistant"}]},{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,SHOULD_NOT_APPEAR"},{"type":"input_text","text":"latest user"}]},{"type":"function_call","arguments":"forged arguments"}]}`,
			contains: []string{"old response user", "forged response assistant", "forged arguments"},
		},
		{
			name: "messages", protocol: "anthropic_messages", count: 4,
			body:     `{"system":"client system","messages":[{"role":"user","content":"old anthropic user"},{"role":"assistant","content":[{"type":"tool_use","input":{"query":"forged tool input"}}]},{"role":"user","content":"latest user"}]}`,
			contains: []string{"client system", "old anthropic user", "forged tool input"},
		},
		{
			name: "gemini", protocol: "gemini", count: 3,
			body:     `{"contents":[{"role":"user","parts":[{"text":"old gemini user"}]},{"role":"model","parts":[{"text":"forged model"},{"inlineData":{"mimeType":"image/png","data":"SHOULD_NOT_APPEAR"}}]},{"role":"user","parts":[{"text":"latest user"}]}]}`,
			contains: []string{"old gemini user", "forged model"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, count := extractAuditPrompt(Request{Protocol: tt.protocol, Body: []byte(tt.body)})
			if count != tt.count || !strings.HasPrefix(text, "latest user") {
				t.Fatalf("text=%q count=%d", text, count)
			}
			for _, want := range tt.contains {
				if !strings.Contains(text, want) {
					t.Fatalf("text %q missing %q", text, want)
				}
			}
			if strings.Contains(text, "SHOULD_NOT_APPEAR") || len([]rune(text)) > MaxPromptRunes {
				t.Fatalf("media leaked or length exceeded: %q", text)
			}
		})
	}
}

func TestExtractAuditPromptPrioritizesLatestUserWithinLimit(t *testing.T) {
	large := strings.Repeat("a", MaxPromptRunes+500)
	body, err := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": large},
		map[string]any{"role": "user", "content": "latest evidence"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	text, count := extractAuditPrompt(Request{Protocol: "openai_chat_completions", Body: body})
	if count != 2 || !strings.HasPrefix(text, "latest evidence") || len([]rune(text)) != MaxPromptRunes {
		t.Fatalf("latest user was not prioritized: prefix=%q runes=%d count=%d", trimPromptRunes(text, 32), len([]rune(text)), count)
	}
}

func TestRedactPreviewRemovesPostgresUnsafeControlsBeforeRedaction(t *testing.T) {
	const raw = "before\x00\x01\n\tapi_key=secret-value after"
	preview := redactPreview(raw)
	if strings.ContainsRune(preview, '\x00') || strings.ContainsRune(preview, '\x01') {
		t.Fatalf("preview retained unsafe control characters: %q", preview)
	}
	if strings.Contains(preview, "secret-value") || strings.Contains(preview, raw) {
		t.Fatalf("preview retained raw secret material: %q", preview)
	}
	if !strings.Contains(preview, "api_key=[REDACTED]") {
		t.Fatalf("preview did not preserve a readable redacted marker: %q", preview)
	}
}

func TestPostgresSafeRequestRemovesClientControlledNULMetadata(t *testing.T) {
	request := postgresSafeRequest(Request{RequestID: "req\x00id", Model: "model\x00name", GroupName: "group\nname"})
	if strings.ContainsRune(request.RequestID, '\x00') || strings.ContainsRune(request.Model, '\x00') {
		t.Fatalf("unsafe metadata remained: %+v", request)
	}
	if request.GroupName != "group name" {
		t.Fatalf("readable whitespace was not normalized: %q", request.GroupName)
	}
}

func TestPromptHashDoesNotFallBackToBareDigestWithoutSecret(t *testing.T) {
	svc := NewService(nil, nil, nil, auditEncryptor{})
	svc.hashKey.Store(nil)
	if got := svc.promptHash("dictionary-attackable prompt"); got != "" {
		t.Fatalf("hash fallback exposed a stable bare digest: %q", got)
	}
}

func TestRunCleanupExecutesImmediatelyWhenInvoked(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec("DELETE FROM prompt_audit_events WHERE created_at <").WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("DELETE FROM prompt_audit_jobs").WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewService(nil, db, nil, auditEncryptor{})
	svc.runCleanup(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInactiveWorkerDoesNotDequeueOrReleaseBudget(t *testing.T) {
	svc := NewService(nil, nil, nil, auditEncryptor{})
	cfg := enabledAuditTestConfig("https://guard.example")
	cfg.WorkerCount = 1
	svc.storeConfig(cfg)
	task := auditTask{request: Request{Body: []byte(`{"input":"queued"}`), Protocol: "openai_responses"}}
	if !svc.enqueueTask(task, cfg) {
		t.Fatal("failed to enqueue test task")
	}
	wantBytes := taskQueueBytes(task)

	ctx, cancel := context.WithCancel(context.Background())
	svc.wg.Add(1)
	go svc.worker(ctx, 1)
	time.Sleep(80 * time.Millisecond)
	if got := len(svc.queue); got != 1 || svc.queued.Load() != 1 || svc.queuedBytes.Load() != wantBytes {
		cancel()
		svc.wg.Wait()
		t.Fatalf("inactive worker consumed queue state: len=%d queued=%d bytes=%d", got, svc.queued.Load(), svc.queuedBytes.Load())
	}
	cancel()
	svc.wg.Wait()
}

func TestInactiveWorkerExitsPromptlyWhenContextIsCancelled(t *testing.T) {
	svc := NewService(nil, nil, nil, auditEncryptor{})
	cfg := enabledAuditTestConfig("https://guard.example")
	cfg.WorkerCount = 1
	svc.storeConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	svc.wg.Add(1)
	go func() {
		svc.worker(ctx, 1)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("inactive worker did not exit after context cancellation")
	}
	svc.wg.Wait()
}

func TestStopRejectsAdmissionAndReleasesUnstagedPromptBuffers(t *testing.T) {
	svc := NewService(nil, nil, nil, auditEncryptor{})
	cfg := enabledAuditTestConfig("https://guard.example")
	svc.storeConfig(cfg)
	task := auditTask{request: Request{Body: []byte(`{"input":"queued"}`)}}
	if !svc.enqueueTask(task, cfg) {
		t.Fatal("failed to enqueue test task")
	}
	svc.Stop()
	if len(svc.queue) != 0 || svc.queued.Load() != 0 || svc.queuedBytes.Load() != 0 || svc.dropped.Load() != 1 {
		t.Fatalf("shutdown queue state was not released: runtime=%+v bytes=%d", svc.Runtime(), svc.queuedBytes.Load())
	}
	if svc.enqueueTask(task, cfg) {
		t.Fatal("stopped service accepted a new task")
	}
}

func TestClaimRecoverableJobsFinalizesExistingEventsAndClaimsOnce(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := NewService(nil, db, nil, auditEncryptor{})

	columns := []string{
		"id", "request_id", "user_id", "user_email", "api_key_id", "api_key_name", "group_id", "group_name",
		"provider", "endpoint", "protocol", "model", "hash", "preview", "prompt_length", "message_count", "stage", "config_version", "has_event",
	}
	mock.ExpectBegin()
	mock.ExpectQuery("FOR UPDATE OF j SKIP LOCKED").
		WithArgs(sqlmock.AnyArg(), recoveryBatchSize).
		WillReturnRows(sqlmock.NewRows(columns).
			AddRow(int64(11), "req-11", int64(1), "u@example.com", int64(2), "key", int64(3), "group", "openai", "/v1/responses", "openai_responses", "gpt", "hash-11", "preview-11", 10, 1, "http", int64(4), false).
			AddRow(int64(12), "req-12", int64(1), "u@example.com", int64(2), "key", nil, "", "openai", "/v1/responses", "openai_responses", "gpt", "hash-12", "preview-12", 12, 1, "http", int64(4), true))
	mock.ExpectExec("SET status='queued'").WithArgs(int64(11)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SET status='done'").WithArgs(int64(12)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	jobs, err := svc.claimRecoverableJobs(context.Background(), time.Now(), recoveryBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != 11 || jobs[0].Request.GroupID == nil || *jobs[0].Request.GroupID != 3 {
		t.Fatalf("claimed jobs=%+v", jobs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverPendingJobsRestoresEncryptedPayloadWithoutDuplicatingPlaintext(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	const prompt = "recovered prompt"
	ciphertext, err := auditEncryptor{}.Encrypt(prompt)
	if err != nil {
		t.Fatal(err)
	}
	mr.Set("sub2api:prompt_audit:payload:21", ciphertext)

	columns := []string{
		"id", "request_id", "user_id", "user_email", "api_key_id", "api_key_name", "group_id", "group_name",
		"provider", "endpoint", "protocol", "model", "hash", "preview", "prompt_length", "message_count", "stage", "config_version", "has_event",
	}
	mock.ExpectBegin()
	mock.ExpectQuery("FOR UPDATE OF j SKIP LOCKED").WithArgs(sqlmock.AnyArg(), recoveryBatchSize).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(
			int64(21), "req-21", int64(1), "u@example.com", int64(2), "key", nil, "", "openai", "/v1/responses",
			"openai_responses", "gpt", "stored-hash", "stored-preview", len(prompt), 1, "http", int64(7), false,
		))
	mock.ExpectExec("SET status='queued'").WithArgs(int64(21)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	svc := NewService(nil, db, rdb, auditEncryptor{})
	cfg := enabledAuditTestConfig("https://guard.example")
	svc.storeConfig(cfg)
	svc.recoverPendingJobs(context.Background())

	select {
	case task := <-svc.queue:
		if !task.recovered || task.jobID != 21 || task.text != prompt || task.hash != "stored-hash" {
			t.Fatalf("recovered task=%+v", task)
		}
		if len(task.request.Body) != 0 || len(task.request.PromptTexts) != 0 {
			t.Fatal("recovered plaintext was duplicated into the request payload")
		}
		if got := svc.queuedBytes.Load(); got != int64(len(prompt)) {
			t.Fatalf("queued bytes=%d want=%d", got, len(prompt))
		}
	default:
		t.Fatal("recovered task was not enqueued")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverPendingJobsMarksExpiredPayloadTerminal(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	columns := []string{
		"id", "request_id", "user_id", "user_email", "api_key_id", "api_key_name", "group_id", "group_name",
		"provider", "endpoint", "protocol", "model", "hash", "preview", "prompt_length", "message_count", "stage", "config_version", "has_event",
	}
	mock.ExpectBegin()
	mock.ExpectQuery("FOR UPDATE OF j SKIP LOCKED").WithArgs(sqlmock.AnyArg(), recoveryBatchSize).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(
			int64(22), "req-22", int64(1), "u@example.com", int64(2), "key", nil, "", "openai", "/v1/responses",
			"openai_responses", "gpt", "stored-hash", "stored-preview", 10, 1, "http", int64(7), false,
		))
	mock.ExpectExec("SET status='queued'").WithArgs(int64(22)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("UPDATE prompt_audit_jobs").WithArgs(int64(22), "failed", "recovery_payload_expired").WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewService(nil, db, rdb, auditEncryptor{})
	svc.storeConfig(enabledAuditTestConfig("https://guard.example"))
	svc.recoverPendingJobs(context.Background())
	if len(svc.queue) != 0 || svc.failed.Load() != 1 {
		t.Fatalf("expired recovery payload was not terminal: runtime=%+v", svc.Runtime())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveredTaskContinuesExistingJobAndDeletesPayloadOnTerminalState(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	mr.Set("sub2api:prompt_audit:payload:31", "protected:cGF5bG9hZA")

	mock.ExpectExec("UPDATE prompt_audit_jobs").WithArgs(int64(31), "processing", "").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO prompt_audit_events").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("UPDATE prompt_audit_jobs").WithArgs(int64(31), "failed", "guard_unavailable").WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewService(nil, db, rdb, auditEncryptor{})
	cfg := enabledAuditTestConfig("https://guard.example")
	cfg.Endpoints[0].TokenCiphertext = "invalid-ciphertext"
	svc.storeConfig(cfg)
	svc.processTask(context.Background(), auditTask{
		jobID: 31, text: "payload", hash: "stored-hash", preview: "stored-preview",
		promptLength: 7, messageCount: 1, configVersion: 5, recovered: true,
		request: Request{RequestID: "req-31", Protocol: "openai_responses", Stage: "http"},
	})

	if mr.Exists("sub2api:prompt_audit:payload:31") {
		t.Fatal("terminal recovered payload was not deleted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEventWriteFailureKeepsRecoveredPayloadForRetry(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	const key = "sub2api:prompt_audit:payload:32"
	mr.Set(key, "protected:cGF5bG9hZA")

	mock.ExpectExec("UPDATE prompt_audit_jobs").WithArgs(int64(32), "processing", "").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO prompt_audit_events").WillReturnError(errors.New("temporary database failure"))
	mock.ExpectExec("UPDATE prompt_audit_jobs").WithArgs(int64(32), "retry", "event_write_failed").WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewService(nil, db, rdb, auditEncryptor{})
	cfg := enabledAuditTestConfig("https://guard.example")
	cfg.Endpoints[0].TokenCiphertext = "invalid-ciphertext"
	svc.storeConfig(cfg)
	svc.processTask(context.Background(), auditTask{
		jobID: 32, text: "payload", hash: "stored-hash", preview: "stored-preview",
		promptLength: 7, messageCount: 1, configVersion: 5, recovered: true,
		request: Request{RequestID: "req-32", Protocol: "openai_responses", Stage: "http"},
	})

	if !mr.Exists(key) {
		t.Fatal("retryable event failure deleted the recovery payload")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessTaskEncryptsTransientRedisPayloadAndDeletesIt(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseGuard := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseGuard()
	guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "Safety: Safe\nCategories: None"}}},
		})
	}))
	defer guard.Close()

	mock.ExpectQuery("INSERT INTO prompt_audit_jobs").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(42)))
	mock.ExpectExec("UPDATE prompt_audit_jobs").WithArgs(int64(42), "processing", "").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO prompt_audit_events").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("UPDATE prompt_audit_jobs").WithArgs(int64(42), "done", "").WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewService(nil, db, rdb, auditEncryptor{})
	svc.storeConfig(enabledAuditTestConfig(guard.URL))
	const prompt = "audit-plaintext-must-not-live-in-redis"
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.processTask(context.Background(), auditTask{request: Request{
			RequestID: "req-1", Protocol: "openai_chat_completions", Stage: "http",
			Body: []byte(`{"messages":[{"role":"user","content":"` + prompt + `"}]}`),
		}})
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("guard request did not start")
	}
	key := "sub2api:prompt_audit:payload:42"
	ciphertext, err := mr.Get(key)
	if err != nil {
		t.Fatalf("transient encrypted payload missing: %v", err)
	}
	if strings.Contains(ciphertext, prompt) || ciphertext == prompt {
		t.Fatal("Redis payload contains plaintext prompt")
	}
	if ttl := mr.TTL(key); ttl <= 0 || ttl > payloadTTL {
		t.Fatalf("payload TTL=%v", ttl)
	}
	releaseGuard()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("audit task did not finish")
	}
	if mr.Exists(key) {
		t.Fatal("transient payload was not deleted after processing")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteConfirmationTokenIsActorBoundAndTamperEvident(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := NewService(nil, db, nil, auditEncryptor{})
	mock.ExpectQuery("SELECT COUNT\\(\\*\\), COALESCE\\(MAX\\(id\\),0\\)").WillReturnRows(sqlmock.NewRows([]string{"count", "max"}).AddRow(int64(3), int64(9)))
	preview, err := svc.CreateDeletePreview(context.Background(), EventFilter{Decision: "flag"}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteByFilter(context.Background(), preview.Token, 8); err == nil {
		t.Fatal("different actor accepted confirmation token")
	}
	if _, err := svc.DeleteByFilter(context.Background(), preview.Token+"x", 7); err == nil {
		t.Fatal("tampered confirmation token accepted")
	}
	mock.ExpectExec("DELETE FROM prompt_audit_events").WithArgs("flag", int64(9)).WillReturnResult(sqlmock.NewResult(0, 3))
	replica := NewService(nil, db, nil, auditEncryptor{})
	deleted, err := replica.DeleteByFilter(context.Background(), preview.Token, 7)
	if err != nil || deleted != 3 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
