package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/connordoman/thermal/internal/auth"
	"github.com/connordoman/thermal/internal/device"
	"github.com/connordoman/thermal/internal/queue"
	"github.com/connordoman/thermal/internal/render"
	"github.com/connordoman/thermal/internal/store"
)

type harness struct {
	t     *testing.T
	srv   *httptest.Server
	admin string
}

// newHarness starts a server whose printer is target.
func newHarness(t *testing.T, target device.Target) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	keys := auth.NewManager(db.Queries)
	admin, _, err := keys.Create(ctx, "admin", []auth.Scope{auth.ScopeAdmin}, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	dev := device.New(device.Config{Target: target, PaperWidth: 576})
	q := queue.New(db, dev, 0)
	go q.Run(ctx)
	s := &Server{
		Store: db, Keys: keys, Device: dev, Queue: q,
		Images:       render.NewImageLoader(false, 1<<20, time.Second),
		MaxBodyBytes: 1 << 20,
	}
	h := &harness{t: t, srv: httptest.NewServer(s.Handler()), admin: admin}
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) do(method, path, key string, body string) (int, map[string]any, []byte) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		json.Unmarshal(raw, &m)
	}
	return resp.StatusCode, m, raw
}

func (h *harness) newKey(scopes ...string) string {
	h.t.Helper()
	b, _ := json.Marshal(map[string]any{"name": "k", "scopes": scopes})
	code, m, raw := h.do("POST", "/v1/keys", h.admin, string(b))
	if code != http.StatusCreated {
		h.t.Fatalf("creating key: %d %s", code, raw)
	}
	return m["secret"].(string)
}

func field(m map[string]any, path ...string) any {
	var v any = m
	for _, p := range path {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[p]
	}
	return v
}

func TestAuthAndScopes(t *testing.T) {
	h := newHarness(t, device.Target{Kind: device.KindDiscard})
	if code, _, _ := h.do("GET", "/v1/printer", "", ""); code != http.StatusUnauthorized {
		t.Errorf("no key: got %d", code)
	}
	if code, _, _ := h.do("GET", "/v1/printer", "thm_nope_nope", ""); code != http.StatusUnauthorized {
		t.Errorf("bad key: got %d", code)
	}
	reader := h.newKey("read")
	if code, _, _ := h.do("GET", "/v1/printer", reader, ""); code != http.StatusOK {
		t.Errorf("read key on /printer: got %d", code)
	}
	if code, _, _ := h.do("POST", "/v1/print/text", reader, "hi"); code != http.StatusForbidden {
		t.Errorf("read key printing: got %d", code)
	}
	if code, _, _ := h.do("GET", "/v1/keys", reader, ""); code != http.StatusForbidden {
		t.Errorf("read key listing keys: got %d", code)
	}
	if code, _, _ := h.do("GET", "/healthz", "", ""); code != http.StatusOK {
		t.Errorf("healthz: got %d", code)
	}
	if code, _, raw := h.do("GET", "/v1/schema/job.json", "", ""); code != http.StatusOK || !json.Valid(raw) {
		t.Errorf("schema: got %d", code)
	}
}

func TestPrintRoutes(t *testing.T) {
	h := newHarness(t, device.Target{Kind: device.KindDiscard})
	key := h.newKey("print", "read")
	for _, tc := range []struct{ path, body string }{
		{"/v1/print/text?wait=5s", "hello"},
		{"/v1/print/raw?wait=5s", "\x1b@hi\n"},
		{"/v1/print/markdown?wait=5s", "# Hi\n\n- a\n- b"},
		{"/v1/print/utf8?wait=5s", "こんにちは 😀"},
		{"/v1/print/json?wait=5s", `[{"type":"text","content":"hello, world"},{"type":"qr_code","content":"https://example.com"},{"type":"cut","content":"PARTIAL"}]`},
	} {
		code, m, raw := h.do("POST", tc.path, key, tc.body)
		if code != http.StatusOK || field(m, "job", "status") != "completed" {
			t.Errorf("%s: %d %s", tc.path, code, raw)
		}
	}
	code, m, _ := h.do("GET", "/v1/jobs?limit=2", key, "")
	if code != http.StatusOK || len(m["jobs"].([]any)) != 2 || m["next"] == nil {
		t.Errorf("listing jobs: %d %v", code, m)
	}
	code, _, raw := h.do("GET", "/v1/jobs/2/payload", key, "")
	if code != http.StatusOK || string(raw) != "\x1b@hi\n" {
		t.Errorf("raw payload: %d %q", code, raw)
	}
	code, _, raw = h.do("POST", "/v1/print/text?dry_run=true&cut=none", key, "x")
	if code != http.StatusOK || string(raw) != "\x1b@x\n" {
		t.Errorf("dry run: %d %q", code, raw)
	}
	code, m, _ = h.do("POST", "/v1/print/json", key, `{"blocks":[{"type":"barcode","symbology":"EAN13","content":"1"}]}`)
	if code != http.StatusUnprocessableEntity || field(m, "error", "code") != "invalid_block" {
		t.Errorf("bad barcode: %d %v", code, m)
	}
}

func TestQueueCancelAndRetry(t *testing.T) {
	// A printer that cannot be opened keeps jobs queued.
	h := newHarness(t, device.Target{Kind: device.KindFile, Path: filepath.Join(t.TempDir(), "missing")})
	key := h.newKey("print", "read")
	code, m, raw := h.do("POST", "/v1/print/text?priority=5&label=first", key, "one")
	if code != http.StatusAccepted || field(m, "job", "status") != "queued" {
		t.Fatalf("submit: %d %s", code, raw)
	}
	h.do("POST", "/v1/print/text?priority=9", key, "two")
	_, m, _ = h.do("GET", "/v1/queue", key, "")
	queued := m["queued"].([]any)
	if len(queued) != 2 || queued[0].(map[string]any)["id"].(float64) != 2 {
		t.Errorf("queue order: %v", queued)
	}
	if field(m, "worker", "state") != queue.StateOffline {
		t.Errorf("worker state: %v", m["worker"])
	}
	code, m, _ = h.do("POST", "/v1/jobs/1/cancel", key, "")
	if code != http.StatusOK || field(m, "job", "status") != "canceled" {
		t.Errorf("cancel: %d %v", code, m)
	}
	if code, _, _ := h.do("POST", "/v1/jobs/1/cancel", key, ""); code != http.StatusConflict {
		t.Errorf("second cancel: %d", code)
	}
	code, m, _ = h.do("POST", "/v1/jobs/1/retry", key, "")
	if code != http.StatusAccepted || field(m, "job", "retry_of").(float64) != 1 {
		t.Errorf("retry: %d %v", code, m)
	}
	_, m, _ = h.do("GET", "/v1/audit", h.admin, "")
	if len(m["events"].([]any)) < 3 { // key.created, job.canceled, job.retried
		t.Errorf("audit: %v", m)
	}
}

func TestKeyRotationAndRevocation(t *testing.T) {
	h := newHarness(t, device.Target{Kind: device.KindDiscard})
	old := h.newKey("read")
	id, _, _ := auth.Parse(old)

	code, m, raw := h.do("POST", "/v1/keys/"+id+"/rotate", h.admin, `{"grace":"1h"}`)
	if code != http.StatusOK {
		t.Fatalf("rotate: %d %s", code, raw)
	}
	fresh := m["secret"].(string)
	for _, k := range []string{old, fresh} {
		if code, _, _ := h.do("GET", "/v1/whoami", k, ""); code != http.StatusOK {
			t.Errorf("key %s during grace: %d", k[:13], code)
		}
	}
	h.do("POST", "/v1/keys/"+id+"/rotate", h.admin, "")
	if code, _, _ := h.do("GET", "/v1/whoami", fresh, ""); code != http.StatusUnauthorized {
		t.Errorf("key replaced without grace still works: %d", code)
	}

	adminID, _, _ := auth.Parse(h.admin)
	if code, _, _ := h.do("DELETE", "/v1/keys/"+adminID, h.admin, ""); code != http.StatusConflict {
		t.Errorf("revoking the last admin: %d", code)
	}
	if code, _, _ := h.do("PATCH", "/v1/keys/"+adminID, h.admin, `{"scopes":["read"]}`); code != http.StatusConflict {
		t.Errorf("demoting the last admin: %d", code)
	}
	second := h.newKey("admin")
	if code, _, _ := h.do("DELETE", "/v1/keys/"+adminID, second, ""); code != http.StatusOK {
		t.Errorf("revoking an admin with another left: %d", code)
	}
	if code, _, _ := h.do("GET", "/v1/whoami", h.admin, ""); code != http.StatusUnauthorized {
		t.Errorf("revoked key still works: %d", code)
	}
	code, m, _ = h.do("PATCH", "/v1/keys/"+id, second, `{"name":"renamed","expires_in":"24h"}`)
	if code != http.StatusOK || field(m, "key", "name") != "renamed" || field(m, "key", "expires_at") == nil {
		t.Errorf("update: %d %v", code, m)
	}
}
