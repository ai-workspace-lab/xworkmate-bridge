package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"xworkmate-bridge/internal/gatewayruntime"
)

func TestLocalSessionAuthFailClosed(t *testing.T) {
	for _, configured := range []string{"", "expected"} {
		t.Run("configured="+configured, func(t *testing.T) {
			t.Setenv("BRIDGE_AUTH_TOKEN", configured)
			t.Setenv("AI_WORKSPACE_AUTH_TOKEN", "")
			t.Setenv("BRIDGE_REVIEW_AUTH_TOKEN", "")
			req := httptest.NewRequest("GET", "/api/v1/namespaces", nil)
			req.Header.Set("Authorization", "Bearer wrong")
			rec := httptest.NewRecorder()
			(&Server{}).handleTaskSessionAPI(rec, req)
			if rec.Code != 401 {
				t.Fatalf("status %d", rec.Code)
			}
		})
	}
}

func TestLocalSessionPostgres17RestartAndIdempotency(t *testing.T) {
	if os.Getenv("BRIDGE_TEST_PG_FIXTURE") != "1" {
		t.Skip("set BRIDGE_TEST_PG_FIXTURE=1 to start isolated PostgreSQL 17 fixture")
	}
	bin, err := exec.LookPath("initdb")
	if err != nil {
		t.Fatal(err)
	}
	// Unix socket names have a small platform limit; macOS test temp paths exceed it.
	root, err := os.MkdirTemp("/tmp", "bridge-pg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	data := filepath.Join(root, "pg")
	socket := filepath.Join(root, "socket")
	if err := os.Mkdir(socket, 0700); err != nil {
		t.Fatal(err)
	}
	command := func(name string, args ...string) {
		t.Helper()
		if output, err := exec.Command(filepath.Join(filepath.Dir(bin), name), args...).CombinedOutput(); err != nil {
			t.Fatalf("%s failed: %s", name, output)
		}
	}
	command("initdb", "-D", data, "-A", "trust", "--no-locale", "-U", "bridge_fixture")
	command("pg_ctl", "-D", data, "-l", filepath.Join(root, "pg.log"), "-o", "-h '' -k "+socket, "-w", "start")
	t.Cleanup(func() { command("pg_ctl", "-D", data, "-m", "fast", "-w", "stop") })
	t.Setenv("BRIDGE_SESSION_PG_URL", "host="+socket+" user=bridge_fixture dbname=postgres sslmode=disable")
	t.Setenv("BRIDGE_SESSION_DIR", filepath.Join(root, "bodies"))
	t.Setenv("BRIDGE_SESSION_OWNER_ID", "")
	t.Setenv("BRIDGE_AUTH_TOKEN", "fixture-only")
	t.Setenv("AI_WORKSPACE_AUTH_TOKEN", "")
	t.Setenv("BRIDGE_REVIEW_AUTH_TOKEN", "")
	call := func(server *Server, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer fixture-only")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.handleTaskSessionAPI(w, r)
		return w
	}
	server := &Server{}
	created := call(server, "POST", "/api/v1/namespaces/personal/sessions", `{"title":"private title","clientRequestId":"create-1"}`)
	if created.Code != 201 {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var snapshot map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	id := snapshot["sessionId"].(string)
	replay := call(server, "POST", "/api/v1/namespaces/personal/sessions", `{"title":"private title","clientRequestId":"create-1"}`)
	if replay.Body.String() != created.Body.String() {
		t.Fatal("idempotent replay changed snapshot")
	}
	if got := call(server, "POST", "/api/v1/namespaces/personal/sessions", `{"title":"different","clientRequestId":"create-1"}`); got.Code != 409 {
		t.Fatal("missing idempotency conflict")
	}
	var version string
	if err := server.localSessionStore.db.QueryRowContext(context.Background(), "SHOW server_version").Scan(&version); err != nil || !strings.HasPrefix(version, "17.") {
		t.Fatalf("expected PostgreSQL17, got %s", version)
	}
	if err := server.localSessionStore.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := &Server{}
	got := call(restarted, "GET", "/api/v1/sessions/"+id, "")
	if got.Body.String() != created.Body.String() {
		t.Fatalf("restart lost snapshot: %s", got.Body.String())
	}
	defer func() {
		if err := restarted.localSessionStore.Close(); err != nil {
			t.Error(err)
		}
	}()
	if got := call(restarted, "GET", "/api/v1/sessions/"+id+"/events?after_seq=0", ""); got.Code != 200 || !strings.Contains(got.Body.String(), `"events":[]`) {
		t.Fatal("empty replay invalid")
	}
	configured := &Server{accountsSessionAPIURL: ":invalid", accountsSessionClient: newAccountsSessionProxyClient()}
	if got := call(configured, "GET", "/api/v1/namespaces", ""); got.Code != 503 || configured.localSessionStore != nil {
		t.Fatal("configured Accounts fell back")
	}
	testLocalExecutionLifecycle(t, restarted, id, call)
}

type localTestProvider struct {
	ProviderCompat
	calls     atomic.Int32
	methodsMu sync.Mutex
	methods   []string
	execute   func(context.Context, map[string]any) (map[string]any, error)
}

func (p *localTestProvider) StartSession(ctx context.Context, session, thread string, params map[string]any, sink SessionNotificationSink) (map[string]any, error) {
	p.calls.Add(1)
	p.methodsMu.Lock()
	p.methods = append(p.methods, "session.start")
	p.methodsMu.Unlock()
	return p.execute(ctx, params)
}
func (p *localTestProvider) SendMessage(ctx context.Context, session, thread string, params map[string]any, sink SessionNotificationSink) (map[string]any, error) {
	p.calls.Add(1)
	p.methodsMu.Lock()
	p.methods = append(p.methods, "session.message")
	p.methodsMu.Unlock()
	return p.execute(ctx, params)
}

func testLocalExecutionLifecycle(t *testing.T, server *Server, id string, call func(*Server, string, string, string) *httptest.ResponseRecorder) {
	t.Helper()
	server.sessions = make(map[string]*session)
	server.routingEngine = fixedTaskRunRoutingEngine{}
	entered := make(chan struct{}, 10)
	release := make(chan struct{})
	cancelObserved := make(chan struct{})
	cancelSettled := make(chan struct{})
	provider := &localTestProvider{execute: func(ctx context.Context, params map[string]any) (map[string]any, error) {
		entered <- struct{}{}
		text, _ := params["taskPrompt"].(string)
		if text == "failure" {
			return nil, fmt.Errorf("fixture executor failed")
		}
		if text == "cancel" {
			<-ctx.Done()
			close(cancelObserved)
			<-cancelSettled
			return nil, ctx.Err()
		}
		if text == "first" {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return map[string]any{"success": true, "status": "completed", "output": "answer: " + text}, nil
	}}
	server.providers = map[string]ProviderCompat{"test-provider": provider}
	server.orchestrator = NewSessionOrchestrator(server)
	decode := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	waitState := func(run, want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var state string
			err := server.localSessionStore.db.QueryRow(`SELECT state FROM bridge_local_runs WHERE id=$1`, run).Scan(&state)
			if err != nil {
				t.Fatal(err)
			}
			if state == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("run did not reach %s", want)
	}
	receiptRun := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		if w.Code != 202 && w.Code != 200 {
			t.Fatalf("message: %d %s", w.Code, w.Body.String())
		}
		return decode(w)["taskRun"].(map[string]any)["id"].(string)
	}
	const duplicates = 8
	responses := make(chan *httptest.ResponseRecorder, duplicates)
	var wg sync.WaitGroup
	for i := 0; i < duplicates; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses <- call(server, "POST", "/api/v1/sessions/"+id+"/messages", `{"clientRequestId":"duplicate","text":"first"}`)
		}()
	}
	wg.Wait()
	close(responses)
	run := ""
	for response := range responses {
		got := receiptRun(response)
		if run != "" && run != got {
			t.Fatal("duplicate created second run")
		}
		run = got
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("executor not called")
	}
	if provider.calls.Load() != 1 {
		t.Fatal("duplicate executed twice")
	}
	if got := call(server, "POST", "/api/v1/sessions/"+id+"/messages", `{"clientRequestId":"duplicate","text":"changed"}`); got.Code != 409 {
		t.Fatal("message conflict not rejected")
	}
	close(release)
	waitState(run, "completed")
	follow := receiptRun(call(server, "POST", "/api/v1/sessions/"+id+"/messages", `{"clientRequestId":"follow","text":"follow-up"}`))
	waitState(follow, "completed")
	provider.methodsMu.Lock()
	if len(provider.methods) != 2 || provider.methods[0] != "session.start" || provider.methods[1] != "session.message" {
		t.Errorf("wrong lifecycle %v", provider.methods)
	}
	provider.methodsMu.Unlock()
	failed := receiptRun(call(server, "POST", "/api/v1/sessions/"+id+"/messages", `{"clientRequestId":"failure","text":"failure"}`))
	waitState(failed, "failed")
	cancelled := receiptRun(call(server, "POST", "/api/v1/sessions/"+id+"/messages", `{"clientRequestId":"cancel","text":"cancel"}`))
	// Wait until the cancellation target is executing, not merely accepted.
	deadline := time.Now().Add(5 * time.Second)
	for provider.calls.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancellation := call(server, "POST", "/api/v1/sessions/"+id+"/runs/"+cancelled+"/cancel", `{"clientRequestId":"cancel-key"}`)
	if cancellation.Code != 200 || decode(cancellation)["contextCancelled"] != true {
		t.Fatalf("cancel did not acknowledge context cancellation: %s", cancellation.Body.String())
	}
	select {
	case <-cancelObserved:
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not observe cancellation")
	}
	var cancelState string
	if err := server.localSessionStore.db.QueryRow(`SELECT state FROM bridge_local_runs WHERE id=$1`, cancelled).Scan(&cancelState); err != nil || cancelState != "cancel_requested" {
		t.Fatal("cancel declared terminal before executor returned")
	}
	close(cancelSettled)
	waitState(cancelled, "cancelled")
	replay := call(server, "POST", "/api/v1/sessions/"+id+"/runs/"+cancelled+"/cancel", `{"clientRequestId":"cancel-key"}`)
	if replay.Code != 200 {
		t.Fatal("cancel replay failed")
	}
	page := decode(call(server, "GET", "/api/v1/sessions/"+id+"/events?after_seq=0&limit=500", ""))
	events := page["events"].([]any)
	types := map[string]int{}
	last := 0
	for _, raw := range events {
		event := raw.(map[string]any)
		seq := int(event["seq"].(float64))
		if seq != last+1 {
			t.Fatal("event gap")
		}
		last = seq
		kind := event["type"].(string)
		types[kind]++
		payload := event["payload"].(map[string]any)
		if payload["taskRunId"] == nil || payload["schemaVersion"] != float64(1) {
			t.Fatal("App event contract missing")
		}
		if kind == "assistant.message.created" && !strings.HasPrefix(payload["text"].(string), "answer:") {
			t.Fatal("answer body missing")
		}
	}
	if types["message.created"] != 4 || types["assistant.message.created"] != 2 || types["run.completed"] != 2 || types["run.failed"] != 1 || types["run.cancelled"] != 1 || types["run.cancel_requested"] != 1 {
		t.Fatalf("unexpected events %v", types)
	}
	paged := decode(call(server, "GET", "/api/v1/sessions/"+id+"/events?after_seq=2&limit=1", ""))
	if len(paged["events"].([]any)) != 1 || paged["lastEventSeq"] != float64(3) {
		t.Fatal("event pagination invalid")
	}
	t.Setenv("BRIDGE_REVIEW_AUTH_TOKEN", "other-owner")
	// Static bearer rotation retains the same installation owner and history.
	r := httptest.NewRequest("GET", "/api/v1/sessions/"+id, nil)
	r.Header.Set("Authorization", "Bearer other-owner")
	w := httptest.NewRecorder()
	server.handleTaskSessionAPI(w, r)
	if w.Code != 200 {
		t.Fatal("bearer rotation changed owner")
	}
	ownerBefore, _ := localSessionOwner("Bearer fixture-only")
	ownerAfter, _ := localSessionOwner("Bearer other-owner")
	if ownerBefore != ownerAfter {
		t.Fatal("credentials used as identity")
	}
	t.Setenv("BRIDGE_AUTH_TOKEN", "replacement-bearer")
	t.Setenv("BRIDGE_REVIEW_AUTH_TOKEN", "")
	rotated := httptest.NewRequest("GET", "/api/v1/sessions/"+id, nil)
	rotated.Header.Set("Authorization", "Bearer replacement-bearer")
	rotatedResponse := httptest.NewRecorder()
	server.handleTaskSessionAPI(rotatedResponse, rotated)
	if rotatedResponse.Code != 200 {
		t.Fatal("replacing old bearer lost session access")
	}
	if old := call(server, "GET", "/api/v1/sessions/"+id, ""); old.Code != 401 {
		t.Fatal("retired bearer still accepted")
	}
	t.Setenv("BRIDGE_AUTH_TOKEN", "fixture-only")
	t.Setenv("BRIDGE_REVIEW_AUTH_TOKEN", "other-owner")
	t.Setenv("BRIDGE_SESSION_OWNER_ID", "different-installation-owner")
	for _, route := range []struct{ method, path, body string }{{"GET", "/api/v1/sessions/" + id, ""}, {"GET", "/api/v1/sessions/" + id + "/events", ""}, {"POST", "/api/v1/sessions/" + id + "/messages", `{"clientRequestId":"denied","text":"hello"}`}, {"POST", "/api/v1/sessions/" + id + "/runs/" + cancelled + "/cancel", `{"clientRequestId":"denied"}`}} {
		r := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
		r.Header.Set("Authorization", "Bearer other-owner")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.handleTaskSessionAPI(w, r)
		if w.Code != 404 {
			t.Fatalf("cross owner route returned %d", w.Code)
		}
	}
	t.Setenv("BRIDGE_SESSION_OWNER_ID", "")
	// Persist an accepted in-flight record without executing it, representing a
	// process crash between commit and dispatch. Recovery must never execute it.
	store := server.localSessionStore
	store.wg.Wait()
	// A second live Bridge must not reconcile or steal this instance's runs.
	if second, err := openLocalSessionStore(context.Background(), os.Getenv("BRIDGE_SESSION_PG_URL"), os.Getenv("BRIDGE_SESSION_DIR")); err == nil {
		second.Close()
		t.Fatal("second instance stole local database ownership")
	}
	for _, table := range []string{"bridge_local_sessions", "bridge_local_runs", "bridge_local_events"} {
		var rowsJSON string
		if err := store.db.QueryRow(`SELECT COALESCE(json_agg(t)::text,'[]') FROM ` + table + ` t`).Scan(&rowsJSON); err != nil {
			t.Fatal(err)
		}
		for _, body := range []string{"private title", "answer: first", "fixture-only"} {
			if strings.Contains(rowsJSON, body) {
				t.Fatalf("body or credential duplicated into PostgreSQL table %s", table)
			}
		}
	}
	_, err := store.db.Exec(`INSERT INTO bridge_local_runs(id,session_id,request_key,request_hash,input_ref,receipt_ref,state) VALUES('interrupted',$1,'crash','hash','unused','unused','running')`, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"queued", "cancel_requested"} {
		newSession := decode(call(server, "POST", "/api/v1/namespaces/personal/sessions", fmt.Sprintf(`{"title":%q}`, state)))["sessionId"].(string)
		if _, err := store.db.Exec(`INSERT INTO bridge_local_runs(id,session_id,request_key,request_hash,input_ref,receipt_ref,state) VALUES($1,$2,'crash','hash','unused','unused',$3)`, "interrupted-"+state, newSession, state); err != nil {
			t.Fatal(err)
		}
	}
	before := provider.calls.Load()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := openLocalSessionStore(context.Background(), os.Getenv("BRIDGE_SESSION_PG_URL"), os.Getenv("BRIDGE_SESSION_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	server.localSessionStore = recovered
	waitState("interrupted", "failed")
	waitState("interrupted-queued", "failed")
	waitState("interrupted-cancel_requested", "failed")
	if retried := receiptRun(call(server, "POST", "/api/v1/sessions/"+id+"/messages", `{"clientRequestId":"duplicate","text":"first"}`)); retried != run {
		t.Fatal("restart changed idempotent receipt")
	}
	if provider.calls.Load() != before {
		t.Fatal("restart reexecuted interrupted task")
	}
	recoveredPage, err := recovered.events(context.Background(), id, url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	all := recoveredPage["events"].([]any)
	terminal := all[len(all)-1].(map[string]any)
	if terminal["type"] != "run.failed" || terminal["payload"].(map[string]any)["code"] != "BRIDGE_RESTART_INTERRUPTED" {
		t.Fatal("restart failure event missing")
	}
	// Verify the production resolver -> orchestrator -> DSH HTTP adapter chain,
	// using a real HTTP executor fixture rather than replacing Process.
	var dshMethodsMu sync.Mutex
	dshMethods := []string{}
	harness := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		method, _ := req["method"].(string)
		dshMethodsMu.Lock()
		dshMethods = append(dshMethods, method)
		dshMethodsMu.Unlock()
		result := map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"success": true, "status": "completed", "output": "DSH fixture answer"}}
		if params, ok := req["params"].(map[string]any); ok && params["prompt"] == "fail DSH" {
			result = map[string]any{"jsonrpc": "2.0", "id": req["id"], "error": map[string]any{"code": -32002, "message": "fixture rejected execution"}}
		}
		if err := json.NewEncoder(w).Encode(result); err != nil {
			t.Error(err)
		}
	}))
	defer harness.Close()
	t.Setenv("BRIDGE_ROUTER_TYPE", "dsh")
	server.gateway = gatewayruntime.NewManager()
	server.providers = map[string]ProviderCompat{"dsh": newDSHAdapter(syncedProvider{Endpoint: harness.URL})}
	server.routingEngine = &DefaultRoutingEngine{server: server}
	for _, key := range []string{"dsh-1", "dsh-2"} {
		run := receiptRun(call(server, "POST", "/api/v1/sessions/"+id+"/messages", fmt.Sprintf(`{"clientRequestId":%q,"text":"real adapter"}`, key)))
		waitState(run, "completed")
	}
	dshMethodsMu.Lock()
	if len(dshMethods) != 2 || dshMethods[0] != "session/prompt" || dshMethods[1] != "session/prompt" {
		t.Errorf("DSH continuation not preserved after restart: %v", dshMethods)
	}
	dshMethodsMu.Unlock()
	fresh := decode(call(server, "POST", "/api/v1/namespaces/personal/sessions", `{"title":"fresh DSH"}`))["sessionId"].(string)
	freshRun := receiptRun(call(server, "POST", "/api/v1/sessions/"+fresh+"/messages", `{"clientRequestId":"fresh-1","text":"real adapter"}`))
	waitState(freshRun, "completed")
	failureRun := receiptRun(call(server, "POST", "/api/v1/sessions/"+fresh+"/messages", `{"clientRequestId":"fresh-2","text":"fail DSH"}`))
	waitState(failureRun, "failed")
	dshMethodsMu.Lock()
	if dshMethods[2] != "session/new" || dshMethods[3] != "session/prompt" {
		t.Errorf("fresh DSH session lifecycle wrong: %v", dshMethods)
	}
	dshMethodsMu.Unlock()
	// A real provider failure must never gain an assistant answer/completion event.
	failurePage := decode(call(server, "GET", "/api/v1/sessions/"+fresh+"/events?limit=500", ""))
	for _, raw := range failurePage["events"].([]any) {
		event := raw.(map[string]any)
		if event["payload"].(map[string]any)["taskRunId"] == failureRun && (event["type"] == "run.completed" || event["type"] == "assistant.message.created") {
			t.Fatal("DSH failure became successful chat")
		}
	}
}

func TestDSHFailuresNeverComplete(t *testing.T) {
	for _, body := range []string{`{"error":{"code":-1}}`, `not json`} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := w.Write([]byte(body)); err != nil {
				t.Error(err)
			}
		}))
		adapter := newDSHAdapter(syncedProvider{Endpoint: upstream.URL})
		sink := func(event map[string]any) {
			if event["event"] == "completed" {
				t.Error("fake completion")
			}
		}
		if _, err := adapter.StartSession(context.Background(), "s", "s", map[string]any{"prompt": "hello"}, sink); err == nil {
			t.Error("start swallowed error")
		}
		if _, err := adapter.SendMessage(context.Background(), "s", "s", nil, sink); err == nil {
			t.Error("message swallowed error")
		}
		if err := adapter.CancelSession(context.Background(), "s"); err == nil {
			t.Error("cancel swallowed error")
		}
		upstream.Close()
	}
}
