package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/store"
	"github.com/Urenzu/trace-portal/internal/trace"
)

func newTestProxy(t *testing.T, upstream http.Handler) (*httptest.Server, *store.Store) {
	t.Helper()

	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	p, err := New(Config{
		Upstream: up.URL,
		Store:    st,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}

	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return front, st
}

// eventsOfType waits briefly for the response event, which is written when the
// client finishes draining the body — a moment after the client's Do returns.
func eventsOfType(t *testing.T, st *store.Store, typ trace.EventType) []trace.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		all, err := st.Events(time.Now().UTC())
		if err != nil {
			t.Fatalf("read events: %v", err)
		}
		var got []trace.Event
		for _, ev := range all {
			if ev.Type == typ {
				got = append(got, ev)
			}
		}
		if len(got) > 0 || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProxyCapturesJSONExchange(t *testing.T) {
	const upstreamBody = `{"id":"msg_1","model":"claude-opus-5","stop_reason":"end_turn",` +
		`"content":[{"type":"text","text":"hello"}],` +
		`"usage":{"input_tokens":11,"output_tokens":3,"cache_read_input_tokens":500}}`

	var gotAuth, gotSession string
	front, st := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Api-Key")
		gotSession = r.Header.Get(SessionHeader)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, upstreamBody)
	}))

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Api-Key", "sk-test")
	req.Header.Set(SessionHeader, "my-session")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The proxy must be transparent in both directions.
	if string(body) != upstreamBody {
		t.Errorf("body altered:\n got %s\nwant %s", body, upstreamBody)
	}
	if gotAuth != "sk-test" {
		t.Errorf("auth header not forwarded, got %q", gotAuth)
	}
	if gotSession != "" {
		t.Errorf("capture header %s leaked upstream: %q", SessionHeader, gotSession)
	}

	events := eventsOfType(t, st, trace.EventResponse)
	if len(events) != 1 {
		t.Fatalf("got %d response events, want 1", len(events))
	}
	ev := events[0]
	if ev.SessionID != "my-session" {
		t.Errorf("SessionID = %q, want the client-supplied one", ev.SessionID)
	}
	if ev.StatusCode != http.StatusOK || ev.StopReason != "end_turn" {
		t.Errorf("response fields: %+v", ev)
	}
	if ev.Usage == nil || ev.Usage.CacheReadInputTokens != 500 {
		t.Errorf("Usage = %+v", ev.Usage)
	}

	// Request and response events pair up, and both blobs round-trip.
	reqs := eventsOfType(t, st, trace.EventRequest)
	if len(reqs) != 1 || reqs[0].TurnID != ev.TurnID {
		t.Fatalf("request/response events did not pair: %+v vs %+v", reqs, ev)
	}
	blob, err := st.GetBlob(ev.ResponseBlob)
	if err != nil {
		t.Fatalf("get response blob: %v", err)
	}
	if string(blob) != upstreamBody {
		t.Errorf("response blob = %s", blob)
	}
}

func TestProxyStreamsSSEIncrementally(t *testing.T) {
	release := make(chan struct{})
	front, st := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)

		io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"model":"claude-opus-5","usage":{"input_tokens":9,"cache_read_input_tokens":80}}}`+"\n\n")
		fl.Flush()

		// Block until the client confirms it received the first frame; if the
		// proxy buffered the stream this test would deadlock.
		<-release

		io.WriteString(w, "event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`+"\n\n")
		fl.Flush()
	}))

	resp, err := http.Post(front.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"go"}]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "message_start") {
			close(release)
			break
		}
	}
	if sc.Err() != nil {
		t.Fatalf("scan stream: %v", sc.Err())
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	events := eventsOfType(t, st, trace.EventResponse)
	if len(events) != 1 {
		t.Fatalf("got %d response events, want 1", len(events))
	}
	ev := events[0]
	if !ev.Stream {
		t.Error("Stream flag not recorded")
	}
	if ev.Usage == nil || ev.Usage.OutputTokens != 42 || ev.Usage.CacheReadInputTokens != 80 {
		t.Errorf("Usage = %+v", ev.Usage)
	}
	if ev.StopReason != "end_turn" {
		t.Errorf("StopReason = %q", ev.StopReason)
	}
}

// Non-Messages endpoints are proxied untouched and generate no events.
func TestProxyIgnoresNonMessagesPaths(t *testing.T) {
	front, st := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[]}`)
	}))

	resp, err := http.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	all, err := st.Events(time.Now().UTC())
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("recorded %d events for a non-Messages path: %+v", len(all), all)
	}
}

func TestProxyRecordsUpstreamFailure(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upURL := up.URL
	up.Close() // nothing is listening, so the proxy's dial fails

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	p, err := New(Config{Upstream: upURL, Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	front := httptest.NewServer(p)
	defer front.Close()

	resp, err := http.Post(front.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}

	events := eventsOfType(t, st, trace.EventError)
	if len(events) != 1 || events[0].Error == "" {
		t.Fatalf("error event not recorded: %+v", events)
	}
}

// Credentials must pass through to the API but never reach disk. This matters
// most for the always-on case, where every request a person makes with their
// real subscription token crosses this proxy.
func TestCredentialsAreForwardedButNeverStored(t *testing.T) {
	const oauthToken = "Bearer sk-ant-oat01-SUPERSECRET-DO-NOT-STORE"
	const apiKey = "sk-ant-api03-ALSO-SECRET"

	var sawAuth, sawKey string
	front, st := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","model":"m","stop_reason":"end_turn",`+
			`"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", oauthToken)
	req.Header.Set("X-Api-Key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Forwarded upstream, or the proxy would break authentication.
	if sawAuth != oauthToken || sawKey != apiKey {
		t.Errorf("credentials not forwarded: auth=%q key=%q", sawAuth, sawKey)
	}

	events := eventsOfType(t, st, trace.EventResponse)
	if len(events) != 1 {
		t.Fatalf("got %d response events, want 1", len(events))
	}

	// Nothing anywhere in the trace directory may contain them.
	var found []string
	root := st.Root()
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		// Blobs are gzipped; check the decompressed form too.
		bodies := [][]byte{raw}
		if zr, zerr := gzip.NewReader(bytes.NewReader(raw)); zerr == nil {
			if plain, derr := io.ReadAll(zr); derr == nil {
				bodies = append(bodies, plain)
			}
			zr.Close()
		}
		for _, b := range bodies {
			if bytes.Contains(b, []byte("SUPERSECRET")) || bytes.Contains(b, []byte("ALSO-SECRET")) {
				found = append(found, path)
				return nil
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk trace dir: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("credentials written to disk in: %v", found)
	}
}

// Real clients negotiate compression, so the API answers with a gzipped
// stream. Parsing the raw bytes finds no usage at all — this is what a stub
// that never compresses fails to catch.
func TestCapturesUsageFromGzippedSSE(t *testing.T) {
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-opus-5","usage":{"input_tokens":14,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":15300}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"bash"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":231}}` + "\n\n"

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(stream))
	zw.Close()
	compressed := gz.Bytes()

	front, st := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(compressed)
	}))

	// Setting Accept-Encoding explicitly is what real clients do, and it is
	// what makes this bug reproducible: Go's transport only auto-decompresses
	// when it added the header itself, so a request that already carries one
	// reaches the capture path still compressed.
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The client must still receive the original compressed bytes untouched.
	if !bytes.Equal(got, compressed) {
		t.Errorf("proxy altered the encoded body: got %d bytes, want %d", len(got), len(compressed))
	}

	events := eventsOfType(t, st, trace.EventResponse)
	if len(events) != 1 {
		t.Fatalf("got %d response events, want 1", len(events))
	}
	ev := events[0]
	if ev.Usage == nil {
		t.Fatal("no usage parsed from a gzipped stream")
	}
	if ev.Usage.CacheReadInputTokens != 15300 || ev.Usage.OutputTokens != 231 {
		t.Errorf("usage = %+v", ev.Usage)
	}
	if ev.StopReason != "tool_use" {
		t.Errorf("StopReason = %q", ev.StopReason)
	}
	if len(ev.ToolCalls) != 1 || ev.ToolCalls[0].Name != "bash" {
		t.Errorf("ToolCalls = %+v", ev.ToolCalls)
	}

	// The stored payload should be readable, not a compressed blob.
	blob, err := st.GetBlob(ev.ResponseBlob)
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	if !bytes.Contains(blob, []byte("message_start")) {
		t.Error("stored payload is not the decoded stream")
	}
}

// An encoding we cannot decode must degrade the trace, never fail the request.
func TestUnknownEncodingDoesNotBreakProxying(t *testing.T) {
	const body = "\x00\x01\x02 not really brotli"
	front, st := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		io.WriteString(w, body)
	}))

	resp, err := http.Post(front.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(got) != body {
		t.Errorf("body altered: %q", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	// The narrow fields still land even though the payload could not be read.
	events := eventsOfType(t, st, trace.EventResponse)
	if len(events) != 1 || events[0].StatusCode != 200 {
		t.Fatalf("events = %+v", events)
	}
}
