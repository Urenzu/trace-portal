package bench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/proxy"
	"github.com/Urenzu/trace-portal/internal/store"
)

// realisticRequest builds an agent-shaped Messages request: a long system
// prompt, a grown message history, and a tool list — the payload an agent loop
// actually resends every turn.
func realisticRequest(historyTurns int) []byte {
	body := map[string]any{
		"model": "claude-opus-5", "max_tokens": 8192, "stream": true,
		"system": []map[string]any{{"type": "text", "text": strings.Repeat("You are a coding agent. ", 400)}},
		"tools": []map[string]any{
			{"name": "bash"}, {"name": "read_file"}, {"name": "edit"}, {"name": "grep"},
		},
	}
	msgs := make([]map[string]any, 0, historyTurns)
	for i := 0; i < historyTurns; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{
			"role": role, "content": strings.Repeat(fmt.Sprintf("turn %d content. ", i), 60),
		})
	}
	body["messages"] = msgs
	raw, _ := json.Marshal(body)
	return raw
}

const upstreamResponse = `{"id":"msg_1","model":"claude-opus-5","stop_reason":"end_turn",` +
	`"content":[{"type":"text","text":"ok"}],` +
	`"usage":{"input_tokens":1200,"output_tokens":400,"cache_read_input_tokens":20000}}`

func percentile(ds []time.Duration, p float64) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	idx := int(float64(len(ds))*p) - 1
	if idx < 0 {
		idx = 0
	}
	return ds[idx]
}

func measure(t *testing.T, target string, body []byte, n int) []time.Duration {
	t.Helper()
	out := make([]time.Duration, 0, n)
	client := &http.Client{}
	for i := 0; i < n; i++ {
		start := time.Now()
		resp, err := client.Post(target+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		out = append(out, time.Since(start))
	}
	return out
}

func TestProxyOverhead(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, upstreamResponse)
	}))
	defer upstream.Close()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	p, err := proxy.New(proxy.Config{
		Upstream: upstream.URL, Store: st,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	defer front.Close()

	const n = 300
	for _, tc := range []struct {
		label   string
		history int
	}{
		{"small  (~10KB)", 4},
		{"medium (~60KB)", 40},
		{"large (~180KB)", 130},
	} {
		body := realisticRequest(tc.history)

		measure(t, upstream.URL, body, 30) // warm up
		direct := measure(t, upstream.URL, body, n)
		measure(t, front.URL, body, 30)
		proxied := measure(t, front.URL, body, n)

		dp50, pp50 := percentile(direct, 0.50), percentile(proxied, 0.50)
		dp99, pp99 := percentile(direct, 0.99), percentile(proxied, 0.99)

		t.Logf("%s payload=%6dB  direct p50=%-8v p99=%-8v | proxied p50=%-8v p99=%-8v | added p50=%-8v p99=%v",
			tc.label, len(body), dp50.Round(time.Microsecond), dp99.Round(time.Microsecond),
			pp50.Round(time.Microsecond), pp99.Round(time.Microsecond),
			(pp50 - dp50).Round(time.Microsecond), (pp99 - dp99).Round(time.Microsecond))
	}
}
