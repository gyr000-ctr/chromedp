package chromedp

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// settleServer starts a test server that serves page at the root. The func
// extra registers more handlers on the mux.
func settleServer(t *testing.T, page string, extra func(*http.ServeMux)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	if extra != nil {
		extra(mux)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, page)
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestSettleNetworkIdle(t *testing.T) {
	t.Parallel()

	ctx, cancel := testAllocate(t, "")
	defer cancel()

	// The page requests six items, one every 250 milliseconds.
	s := settleServer(t, `<!doctype html><html><body>
		<script>
			for (let i = 1; i <= 6; i++) {
				setTimeout(() => fetch('/item?n=' + i), i * 250);
			}
		</script>
	</body></html>`, func(mux *http.ServeMux) {
		mux.HandleFunc("/item", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "ok")
		})
	})

	if err := Do(ctx, Navigate(s.URL)); err != nil {
		t.Fatal(err)
	}
	stats, err := Run(ctx, WaitSettled(
		WithNetworkIdle(400*time.Millisecond),
		WithSettleTimeout(15*time.Second),
	))
	if err != nil {
		t.Fatalf("got error: %v", err)
	}
	if len(stats.Conditions) != 1 || !stats.Conditions[0].Met {
		t.Errorf("the network idle condition is not met: %s", stats)
	}
	if stats.Requests < 1 {
		t.Errorf("got %d requests, want at least 1: %s", stats.Requests, stats)
	}
	if stats.Duration < 400*time.Millisecond {
		t.Errorf("the wait ended after %s, before the quiet window", stats.Duration)
	}
}

func TestSettleLongPolling(t *testing.T) {
	t.Parallel()

	ctx, cancel := testAllocate(t, "")
	defer cancel()

	// The endpoint answers only when the test ends, so a request to it
	// stays open. The page polls it again after each answer. The counter
	// proves that a request is in flight during the whole wait.
	release := make(chan struct{})
	var hanging atomic.Int32
	s := settleServer(t, `<!doctype html><html><body>
		<script>
			function poll() {
				fetch('/poll').then(() => poll()).catch(() => setTimeout(poll, 100));
			}
			poll();
		</script>
	</body></html>`, func(mux *http.ServeMux) {
		mux.HandleFunc("/poll", func(w http.ResponseWriter, r *http.Request) {
			hanging.Add(1)
			defer hanging.Add(-1)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, "ok")
		})
	})
	defer close(release)

	if err := Do(ctx, Navigate(s.URL)); err != nil {
		t.Fatal(err)
	}
	// Make sure that the long poll of the page is in flight.
	for range 100 {
		if hanging.Load() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	start := time.Now()
	stats, err := Run(ctx, WaitSettled(
		WithNetworkIdle(400*time.Millisecond),
		WithSettleTimeout(15*time.Second),
	))
	if err != nil {
		t.Fatalf("got error: %v", err)
	}
	// A request that stays open does not reset the quiet window, so the
	// wait ends long before the endpoint answers.
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("the wait took %s, and the endpoint never answered", d)
	}
	if n := hanging.Load(); n != 1 {
		t.Errorf("got %d requests in flight, want 1: %s", n, stats)
	}
}

func TestSettleWebSocket(t *testing.T) {
	t.Parallel()

	ctx, cancel := testAllocate(t, "")
	defer cancel()

	// The endpoint upgrades the connection to a WebSocket and holds it
	// open. It sends no frames.
	s := settleServer(t, `<!doctype html><html><body>
		<script>
			window.ws = new WebSocket('ws://' + location.host + '/ws');
		</script>
	</body></html>`, func(mux *http.ServeMux) {
		mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "the server cannot hijack the connection", http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			h := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
				base64.StdEncoding.EncodeToString(h[:]))
			io.Copy(io.Discard, conn)
			conn.Close()
		})
	})

	if err := Do(ctx, Navigate(s.URL)); err != nil {
		t.Fatal(err)
	}
	// An open WebSocket does not block the network idle condition.
	stats, err := Run(ctx, WaitSettled(
		WithNetworkIdle(400*time.Millisecond),
		WithSettleTimeout(15*time.Second),
	))
	if err != nil {
		t.Fatalf("got error: %v", err)
	}
	if !stats.Conditions[0].Met {
		t.Errorf("the network idle condition is not met: %s", stats)
	}
}

func TestSettleRequestStorm(t *testing.T) {
	t.Parallel()

	ctx, cancel := testAllocate(t, "")
	defer cancel()

	// The page requests the endpoint again as soon as an answer arrives, so
	// the network never turns quiet.
	s := settleServer(t, `<!doctype html><html><body>
		<script>
			let n = 0;
			function storm() {
				fetch('/fast?n=' + (n++)).then(storm).catch(() => setTimeout(storm, 20));
			}
			storm();
		</script>
	</body></html>`, func(mux *http.ServeMux) {
		mux.HandleFunc("/fast", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "ok")
		})
	})

	if err := Do(ctx, Navigate(s.URL)); err != nil {
		t.Fatal(err)
	}
	stats, err := Run(ctx, WaitSettled(
		WithNetworkIdle(500*time.Millisecond),
		WithSettleTimeout(1500*time.Millisecond),
	))
	if !errors.Is(err, ErrSettleTimeout) {
		t.Fatalf("got error %v, want ErrSettleTimeout", err)
	}
	var terr *SettleTimeoutError
	if !errors.As(err, &terr) {
		t.Fatalf("got error %T, want a SettleTimeoutError", err)
	}
	if len(terr.Pending) != 1 || terr.Pending[0] != "network idle" {
		t.Errorf("got pending %v, want [network idle]", terr.Pending)
	}
	if stats.Requests < 5 {
		t.Errorf("got %d requests, want at least 5: %s", stats.Requests, stats)
	}
	if stats.Duration < 1500*time.Millisecond {
		t.Errorf("the wait ended after %s, before its timeout", stats.Duration)
	}
	t.Logf("stats: %s", stats)
}

func TestSettleAnimation(t *testing.T) {
	t.Parallel()

	ctx, cancel := testAllocate(t, "")
	defer cancel()

	// The page animates the width of a box every 30 milliseconds, for ever.
	// The animation mutates the style attribute, and never the tree.
	s := settleServer(t, `<!doctype html><html><body>
		<div id="box" style="width:10px;height:10px;background:red"></div>
		<script>
			let w = 10;
			setInterval(() => {
				w = (w + 1) % 100;
				document.getElementById('box').style.width = w + 'px';
			}, 30);
		</script>
	</body></html>`, nil)

	if err := Do(ctx, Navigate(s.URL)); err != nil {
		t.Fatal(err)
	}

	t.Run("AttributesIgnored", func(t *testing.T) {
		stats, err := Run(ctx, WaitSettled(
			WithDOMStable(300*time.Millisecond),
			WithSettleTimeout(15*time.Second),
		))
		if err != nil {
			t.Fatalf("got error: %v", err)
		}
		if !stats.Conditions[0].Met {
			t.Errorf("the dom stable condition is not met: %s", stats)
		}
	})

	t.Run("AttributesCounted", func(t *testing.T) {
		stats, err := Run(ctx, WaitSettled(
			WithDOMStable(300*time.Millisecond),
			WithDOMStableAttributes(),
			WithSettleTimeout(1*time.Second),
		))
		if !errors.Is(err, ErrSettleTimeout) {
			t.Fatalf("got error %v, want ErrSettleTimeout", err)
		}
		if stats.Mutations < 5 {
			t.Errorf("got %d mutations, want at least 5: %s", stats.Mutations, stats)
		}
	})
}

func TestSettleDOMStorm(t *testing.T) {
	t.Parallel()

	ctx, cancel := testAllocate(t, "")
	defer cancel()

	// The page adds a node every 50 milliseconds, so the DOM never turns
	// stable.
	s := settleServer(t, `<!doctype html><html><body>
		<script>
			setInterval(() => document.body.appendChild(document.createElement('div')), 50);
		</script>
	</body></html>`, nil)

	if err := Do(ctx, Navigate(s.URL)); err != nil {
		t.Fatal(err)
	}
	stats, err := Run(ctx, WaitSettled(
		WithDOMStable(300*time.Millisecond),
		WithSettleTimeout(1*time.Second),
	))
	if !errors.Is(err, ErrSettleTimeout) {
		t.Fatalf("got error %v, want ErrSettleTimeout", err)
	}
	var terr *SettleTimeoutError
	if !errors.As(err, &terr) {
		t.Fatalf("got error %T, want a SettleTimeoutError", err)
	}
	if len(terr.Pending) != 1 || terr.Pending[0] != "dom stable" {
		t.Errorf("got pending %v, want [dom stable]", terr.Pending)
	}
	if stats.Mutations < 5 {
		t.Errorf("got %d mutations, want at least 5: %s", stats.Mutations, stats)
	}
}

func TestSettleAllConditions(t *testing.T) {
	t.Parallel()

	ctx, cancel := testAllocate(t, "")
	defer cancel()

	// The page shows the element and sets the flag after 600 milliseconds.
	s := settleServer(t, `<!doctype html><html><body>
		<div id="app" style="display:none">ready</div>
		<script>
			setTimeout(() => {
				document.getElementById('app').style.display = 'block';
				window.appReady = true;
			}, 600);
		</script>
	</body></html>`, nil)

	if err := Do(ctx, Navigate(s.URL)); err != nil {
		t.Fatal(err)
	}
	stats, err := Run(ctx, WaitSettled(
		WithNetworkIdle(200*time.Millisecond),
		WithDOMStable(200*time.Millisecond),
		WithElementVisible(CSS("#app")),
		WithJSCondition("window.appReady === true"),
		WithSettleTimeout(15*time.Second),
	))
	if err != nil {
		t.Fatalf("got error: %v", err)
	}
	if len(stats.Conditions) != 4 {
		t.Fatalf("got %d conditions, want 4", len(stats.Conditions))
	}
	for _, c := range stats.Conditions {
		if !c.Met {
			t.Errorf("the condition %q is not met: %s", c.Name, stats)
		}
	}
	if stats.Duration < 500*time.Millisecond {
		t.Errorf("the wait ended after %s, before the page was ready", stats.Duration)
	}
	if !strings.Contains(stats.String(), "network idle") {
		t.Errorf("the summary %q names no network idle condition", stats.String())
	}
	t.Logf("stats: %s", stats)
}

func TestSettleTimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := testAllocate(t, "")
	defer cancel()
	// Make the test fail in time, and not hang, when the wait does not stop.
	ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	s := settleServer(t, `<!doctype html><html><body>never ready</body></html>`, nil)
	if err := Do(ctx, Navigate(s.URL)); err != nil {
		t.Fatal(err)
	}
	_, err := Run(ctx, WaitSettled(
		WithJSCondition("false"),
		WithSettleTimeout(800*time.Millisecond),
	))
	if !errors.Is(err, ErrSettleTimeout) {
		t.Fatalf("got error %v, want ErrSettleTimeout", err)
	}
	var terr *SettleTimeoutError
	if !errors.As(err, &terr) {
		t.Fatalf("got error %T, want a SettleTimeoutError", err)
	}
	if len(terr.Pending) != 1 || terr.Pending[0] != "js false" {
		t.Errorf("got pending %v, want [js false]", terr.Pending)
	}
	if terr.Stats.Duration < 800*time.Millisecond {
		t.Errorf("the wait ended after %s, before its timeout", terr.Stats.Duration)
	}
	// The target still works after a timeout of the wait.
	if _, err := Run(ctx, Title()); err != nil {
		t.Errorf("the target does not work after the timeout: %v", err)
	}
}

func TestSettleCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := testAllocate(t, "")
	defer cancel()

	s := settleServer(t, `<!doctype html><html><body>cancel</body></html>`, nil)
	if err := Do(ctx, Navigate(s.URL)); err != nil {
		t.Fatal(err)
	}
	wctx, wcancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(300 * time.Millisecond)
		wcancel()
	}()
	// The timeout of 0 disables the limit of the wait, so only the
	// cancellation stops it.
	_, err := Run(wctx, WaitSettled(WithJSCondition("false"), WithSettleTimeout(0)))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got error %v, want context.Canceled", err)
	}
	var terr *SettleTimeoutError
	if errors.As(err, &terr) {
		t.Fatalf("got a SettleTimeoutError for a canceled context: %v", err)
	}
}

func TestSettleNoConditions(t *testing.T) {
	t.Parallel()

	ctx, cancel := testAllocate(t, "")
	defer cancel()

	stats, err := Run(ctx, WaitSettled())
	if err != nil {
		t.Fatalf("got error: %v", err)
	}
	if len(stats.Conditions) != 0 {
		t.Errorf("got %d conditions, want 0", len(stats.Conditions))
	}
}
