// Command forge is the entrypoint and lifecycle of the Go-binary forge core,
// a faithful Go port of the Node entry's main.ts.
//
// Trial mode (MAKEFUN_ENV_URL is set): bind an ephemeral port — never 8080, that is
// the environment's own port — register it, then poll GET $MAKEFUN_ENV_URL/done until
// it says {"done": true}, and exit 0. Never hold a request open; never exit before
// done is true. If MAKEFUN_ENV_URL is unset, exit 1 — unless LOCAL_DEV=1, which
// starts the forge on a local port for grading and keeps it running.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"forge/internal/app"
	"forge/internal/scenario"
)

func sleep(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

func register(envURL string, port int) error {
	var lastErr error
	client := &http.Client{Timeout: 5 * time.Second}
	for attempt := 0; attempt < 10; attempt++ {
		body, _ := json.Marshal(map[string]int{"port": port})
		resp, err := client.Post(envURL+"/register", "application/json", bytes.NewReader(body))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusConflict {
				return nil
			}
			lastErr = fmt.Errorf("register failed with HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		sleep(200 * (attempt + 1))
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("register failed")
	}
	return lastErr
}

func waitForDone(envURL string) {
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		resp, err := client.Get(envURL + "/done")
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil {
				var parsed struct {
					Done bool `json:"done"`
				}
				if json.Unmarshal(body, &parsed) == nil && parsed.Done {
					return
				}
			}
		}
		// A transient env hiccup must not kill the entry; keep polling.
		sleep(500)
	}
}

func main() {
	envURL := os.Getenv("MAKEFUN_ENV_URL")
	localDev := os.Getenv("LOCAL_DEV") == "1"

	if envURL == "" && !localDev {
		fmt.Fprintln(os.Stderr, "MAKEFUN_ENV_URL is not set")
		os.Exit(1)
	}

	// Scenario self-check: the port must reproduce the kit's published users.
	scenario.Verify()

	forge := app.NewForge()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	server := &http.Server{Handler: forge.Handler()}

	if localDev && envURL == "" {
		token := app.AuthToken()
		fmt.Printf("makefun-forge listening on http://127.0.0.1:%d\n", port)
		if token == "" {
			fmt.Println("no MAKEFUN_SHOP_TOKEN set: any non-empty Bearer token is accepted")
		} else {
			fmt.Println("MAKEFUN_SHOP_TOKEN is set")
		}
		// Keep running until killed.
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if err := register(envURL, port); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	go func() {
		// Serve in the background while the main goroutine polls /done.
		_ = server.Serve(listener)
	}()
	waitForDone(envURL)
	_ = server.Close()
	os.Exit(0)
}
