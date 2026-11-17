// Full-stack HTTP microbenchmarks for the REST read/write hot paths
// (Builder B): real net/http server + keep-alive client, as the graded
// workloads run them (sequential requests, one warm connection).
package app_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/internal/app"
)

func benchHTTPServer(t *testing.B) (*httptest.Server, *http.Client) {
	t.Helper()
	f := app.NewForge()
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)
	client := srv.Client()
	return srv, client
}

func benchHTTPDo(t *testing.B, client *http.Client, method, url, body string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer bench-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		t.Fatalf("%s %s: status %d", method, url, resp.StatusCode)
	}
}

// BenchmarkHTTPRead: graded read workload over real HTTP.
func BenchmarkHTTPRead(b *testing.B) {
	srv, client := benchHTTPServer(b)
	base := srv.URL + "/api/v3/repos/adaiversen/bench-read"
	benchHTTPDo(b, client, "POST", srv.URL+"/api/v3/user/repos", `{"name":"bench-read"}`)
	benchHTTPDo(b, client, "POST", base+"/issues",
		`{"title":"Fix login redirect loop on mobile Safari","body":"When the session cookie expires mid-flow the app redirects to /login, which bounces back to the dashboard and loops forever."}`)
	getURL := base + "/issues/1"
	// Warm-ups (5, like the workload).
	for i := 0; i < 5; i++ {
		benchHTTPDo(b, client, "GET", getURL, "")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchHTTPDo(b, client, "GET", getURL, "")
	}
}

// BenchmarkHTTPWrite: graded write workload over real HTTP.
func BenchmarkHTTPWrite(b *testing.B) {
	srv, client := benchHTTPServer(b)
	postURL := srv.URL + "/api/v3/repos/adaiversen/bench-write/issues"
	benchHTTPDo(b, client, "POST", srv.URL+"/api/v3/user/repos", `{"name":"bench-write"}`)
	payloads := make([]string, 200)
	for i := range payloads {
		payloads[i] = fmt.Sprintf(`{"title":"Fix login redirect loop on mobile Safari [bench %d]","body":"When the session cookie expires mid-flow the app redirects to /login, which bounces back to the dashboard and loops forever."}`, i+1)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%200 == 0 {
			// Recreate the repo every 200 creates, as the workload does.
			req, _ := http.NewRequest("DELETE", srv.URL+"/api/v3/repos/adaiversen/bench-write", nil)
			req.Header.Set("Authorization", "Bearer bench-token")
			resp, err := client.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			benchHTTPDo(b, client, "POST", srv.URL+"/api/v3/user/repos", `{"name":"bench-write"}`)
		}
		benchHTTPDo(b, client, "POST", postURL, payloads[i%200])
	}
}
