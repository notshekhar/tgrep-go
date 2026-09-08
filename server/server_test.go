package server_test

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/notshekhar/tgrep-go/index"
	"github.com/notshekhar/tgrep-go/live"
	"github.com/notshekhar/tgrep-go/reader"
	"github.com/notshekhar/tgrep-go/server"
)

// start brings up a server on an OS-chosen port over a small corpus.
func start(t *testing.T) (*server.Server, string) {
	t.Helper()

	root := t.TempDir()
	files := map[string]string{
		"a.go":     "package main\n\nfunc handler() {}\n",
		"b.go":     "func handler2() {}\nfunc other() {}\n",
		"c.txt":    "no match here\n",
		"sub/d.go": "func handler3() {}\n",
	}
	var paths []string
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	// Sorted, because file ids are positional and the tests below name paths.
	for i := range paths {
		for j := i + 1; j < len(paths); j++ {
			if paths[j] < paths[i] {
				paths[i], paths[j] = paths[j], paths[i]
			}
		}
	}

	dir := t.TempDir()
	if _, err := index.BuildFromPaths(dir, root, paths, index.Options{Complete: true}); err != nil {
		t.Fatalf("build: %v", err)
	}
	idx, err := reader.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	srv := server.New(root, dir, idx)
	// Port 0 lets the OS choose, so concurrent test runs never collide.
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() {
		srv.Shutdown()
		idx.Close()
	})
	return srv, dir
}

// call sends one request and returns the decoded response.
func call(t *testing.T, addr string, request any) map[string]any {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(encoded, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	if !scanner.Scan() {
		t.Fatalf("no response: %v", scanner.Err())
	}
	var response map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
		t.Fatalf("decode %q: %v", scanner.Text(), err)
	}
	return response
}

func result(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	if e, isError := response["error"]; isError {
		t.Fatalf("unexpected error response: %v", e)
	}
	r, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result object: %v", response)
	}
	return r
}

func TestSearchReturnsMatches(t *testing.T) {
	srv, _ := start(t)

	res := result(t, call(t, srv.Addr(), map[string]any{
		"jsonrpc": "2.0",
		"method":  "search",
		"params":  map[string]any{"pattern": "handler", "detail": true},
		"id":      1,
	}))

	matches, ok := res["matches"].([]any)
	if !ok {
		t.Fatalf("matches is not an array: %v", res["matches"])
	}
	if len(matches) != 3 {
		t.Errorf("got %d matches, want 3 (a.go, b.go, sub/d.go)", len(matches))
	}

	seen := map[string]bool{}
	for _, m := range matches {
		row := m.(map[string]any)
		seen[row["file"].(string)] = true
		if row["type"] != "match" {
			t.Errorf("row type = %v, want match", row["type"])
		}
		// detail:true must carry the spans and columns.
		if row["spans"] == nil || row["columns"] == nil {
			t.Errorf("detail was requested but %v carries no spans/columns", row["file"])
		}
	}
	for _, want := range []string{"a.go", "b.go", "sub/d.go"} {
		if !seen[want] {
			t.Errorf("%s did not appear in the results", want)
		}
	}
}

// TestDetailIsOptional covers the size lever: without it the reply carries no
// spans or columns, which is what keeps a large result set small.
func TestDetailIsOptional(t *testing.T) {
	srv, _ := start(t)

	res := result(t, call(t, srv.Addr(), map[string]any{
		"jsonrpc": "2.0", "method": "search", "id": 1,
		"params": map[string]any{"pattern": "handler"},
	}))
	for _, m := range res["matches"].([]any) {
		row := m.(map[string]any)
		if row["spans"] != nil || row["columns"] != nil {
			t.Errorf("detail was not requested but %v carries spans/columns", row["file"])
		}
	}
}

func TestSearchOptionsAreHonoured(t *testing.T) {
	srv, _ := start(t)

	cases := []struct {
		name   string
		params map[string]any
		want   int
	}{
		{"plain", map[string]any{"pattern": "handler"}, 3},
		{"case insensitive", map[string]any{"pattern": "HANDLER", "case_insensitive": true}, 3},
		{"case sensitive finds nothing", map[string]any{"pattern": "HANDLER"}, 0},
		{"word boundary", map[string]any{"pattern": "handler", "word_boundary": true}, 1},
		{"fixed string", map[string]any{"pattern": "handler(", "fixed_string": true}, 1},
		{"files only", map[string]any{"pattern": "handler", "files_only": true}, 3},
		// b.go has two ADJACENT matching lines; a limit of 1 must keep only
		// the first, so the total is one row per file rather than four.
		{"max count", map[string]any{"pattern": "func", "max_count": 1}, 3},
		{"max count two", map[string]any{"pattern": "func", "max_count": 2}, 4},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := result(t, call(t, srv.Addr(), map[string]any{
				"jsonrpc": "2.0", "method": "search", "id": 1, "params": tc.params,
			}))
			if got := len(res["matches"].([]any)); got != tc.want {
				t.Errorf("got %d rows, want %d", got, tc.want)
			}
		})
	}
}

func TestContextRowsAreLabelled(t *testing.T) {
	srv, _ := start(t)

	res := result(t, call(t, srv.Addr(), map[string]any{
		"jsonrpc": "2.0", "method": "search", "id": 1,
		"params": map[string]any{"pattern": "handler", "before_context": 1, "after_context": 1},
	}))

	var matches, contexts int
	for _, m := range res["matches"].([]any) {
		switch m.(map[string]any)["type"] {
		case "match":
			matches++
		case "context":
			contexts++
		}
	}
	if matches != 3 {
		t.Errorf("got %d match rows, want 3", matches)
	}
	// a.go has a blank line before its match; the others have neighbours too.
	if contexts == 0 {
		t.Error("context was requested but no context rows came back")
	}
}

func TestFilesMethod(t *testing.T) {
	srv, _ := start(t)
	res := result(t, call(t, srv.Addr(), map[string]any{"jsonrpc": "2.0", "method": "files", "id": 1}))

	files, ok := res["files"].([]any)
	if !ok {
		t.Fatalf("files is not an array: %v", res["files"])
	}
	if len(files) != 4 {
		t.Errorf("got %d files, want 4", len(files))
	}
}

func TestStatusMethod(t *testing.T) {
	srv, dir := start(t)
	res := result(t, call(t, srv.Addr(), map[string]any{"jsonrpc": "2.0", "method": "status", "id": 1}))

	if got := res["num_files"].(float64); got != 4 {
		t.Errorf("num_files = %v, want 4", got)
	}
	if res["num_trigrams"].(float64) == 0 {
		t.Error("num_trigrams is zero")
	}
	if res["indexing"].(bool) {
		t.Error("indexing is true for a finished index")
	}
	if res["index_dir"].(string) != dir {
		t.Errorf("index_dir = %v, want %v", res["index_dir"], dir)
	}
}

func TestReloadReopensTheIndex(t *testing.T) {
	srv, _ := start(t)
	res := result(t, call(t, srv.Addr(), map[string]any{"jsonrpc": "2.0", "method": "reload", "id": 1}))
	if got := res["num_files"].(float64); got != 4 {
		t.Errorf("num_files after reload = %v, want 4", got)
	}
	// The server must still answer afterwards — a reload that closed the live
	// mapping without replacing it would fault or return nothing here.
	res = result(t, call(t, srv.Addr(), map[string]any{
		"jsonrpc": "2.0", "method": "search", "id": 2,
		"params": map[string]any{"pattern": "handler"},
	}))
	if len(res["matches"].([]any)) != 3 {
		t.Errorf("search after reload returned %d rows, want 3", len(res["matches"].([]any)))
	}
}

func TestProtocolErrors(t *testing.T) {
	srv, _ := start(t)

	t.Run("unknown method", func(t *testing.T) {
		response := call(t, srv.Addr(), map[string]any{"jsonrpc": "2.0", "method": "nope", "id": 7})
		e, ok := response["error"].(map[string]any)
		if !ok {
			t.Fatalf("expected an error, got %v", response)
		}
		if e["code"].(float64) != -32601 {
			t.Errorf("code = %v, want -32601 (method not found)", e["code"])
		}
		// The id must be echoed, or a client multiplexing requests cannot
		// match the failure to the call that caused it.
		if response["id"].(float64) != 7 {
			t.Errorf("id = %v, want 7", response["id"])
		}
	})

	t.Run("missing pattern", func(t *testing.T) {
		response := call(t, srv.Addr(), map[string]any{
			"jsonrpc": "2.0", "method": "search", "id": 8, "params": map[string]any{},
		})
		e, ok := response["error"].(map[string]any)
		if !ok {
			t.Fatalf("expected an error, got %v", response)
		}
		if e["code"].(float64) != -32602 {
			t.Errorf("code = %v, want -32602 (invalid params)", e["code"])
		}
	})

	t.Run("unsupported pattern", func(t *testing.T) {
		response := call(t, srv.Addr(), map[string]any{
			"jsonrpc": "2.0", "method": "search", "id": 9,
			"params": map[string]any{"pattern": `(a)\1`},
		})
		if _, ok := response["error"].(map[string]any); !ok {
			t.Fatalf("a backreference was accepted: %v", response)
		}
	})
}

// TestMalformedRequestGetsAParseError checks the one case that cannot echo an
// id, since the id could not be read.
func TestMalformedRequestGetsAParseError(t *testing.T) {
	srv, _ := start(t)
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("this is not json\n")); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no response to a malformed request")
	}
	var response map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
		t.Fatalf("the error response is itself malformed: %q", scanner.Text())
	}
	e := response["error"].(map[string]any)
	if e["code"].(float64) != -32700 {
		t.Errorf("code = %v, want -32700 (parse error)", e["code"])
	}
	if response["id"] != nil {
		t.Errorf("id = %v, want null when the request could not be parsed", response["id"])
	}
}

// TestConnectionHandlesManyRequests is the point of a persistent socket: a
// client pays the connection cost once and then queries repeatedly.
func TestConnectionHandlesManyRequests(t *testing.T) {
	srv, _ := start(t)
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for i := range 20 {
		req, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "method": "search", "id": i,
			"params": map[string]any{"pattern": "handler"},
		})
		if _, err := conn.Write(append(req, '\n')); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if !scanner.Scan() {
			t.Fatalf("request %d: no response", i)
		}
		var response map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if got := response["id"].(float64); int(got) != i {
			t.Errorf("request %d got reply for id %v — responses are out of order", i, got)
		}
	}
}

// TestConcurrentClients checks that several sockets can be served at once,
// which is what the server exists for.
func TestConcurrentClients(t *testing.T) {
	srv, _ := start(t)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", srv.Addr())
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			req, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "method": "search", "id": 1,
				"params": map[string]any{"pattern": "handler"},
			})
			if _, err := conn.Write(append(req, '\n')); err != nil {
				errs <- err
				return
			}
			scanner := bufio.NewScanner(conn)
			scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
			if !scanner.Scan() {
				errs <- scanner.Err()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent client: %v", err)
		}
	}
}

// TestPortFileLifecycle covers how a client finds a running server, and why a
// stopped one must not leave the file behind.
func TestPortFileLifecycle(t *testing.T) {
	srv, dir := start(t)

	port, err := server.ReadPort(dir)
	if err != nil {
		t.Fatalf("no port file after Listen: %v", err)
	}
	_, expected, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if port != expected {
		t.Errorf("port file says %q, server is on %q", port, expected)
	}

	if err := srv.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, err := server.ReadPort(dir); err == nil {
		t.Error("the port file survived shutdown, so the next client would dial a dead socket")
	}
}

// TestServerSeesLiveEdits is the whole point of the watcher: an edit made
// after the server started must be searchable without a rebuild or a reload.
//
// The waiting is unavoidable — the watcher debounces — so this polls for the
// expected answer rather than sleeping a fixed amount, and reports what it
// last saw if the answer never arrives.
func TestServerSeesLiveEdits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("func original() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if _, err := index.BuildFromPaths(dir, root, []string{"a.go"}, index.Options{Complete: true}); err != nil {
		t.Fatalf("build: %v", err)
	}
	idx, err := reader.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	srv := server.New(root, dir, idx)
	if err := srv.Watch(live.WatchOptions{Debounce: 20 * time.Millisecond}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Shutdown(); idx.Close() })

	countMatches := func(pattern string) int {
		res := result(t, call(t, srv.Addr(), map[string]any{
			"jsonrpc": "2.0", "method": "search", "id": 1,
			"params": map[string]any{"pattern": pattern},
		}))
		return len(res["matches"].([]any))
	}

	// The index as built knows the original content.
	if got := countMatches("original"); got != 1 {
		t.Fatalf("the freshly built index reports %d matches for the original content, want 1", got)
	}

	t.Run("a new file becomes searchable", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("func appeared() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		waitForCount(t, func() int { return countMatches("appeared") }, 1, "a new file to become searchable")
	})

	t.Run("an edit replaces the indexed content", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("func rewritten() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		waitForCount(t, func() int { return countMatches("rewritten") }, 1, "the edit to become searchable")
		// And the stale content must be gone, which is the half a naive
		// overlay gets wrong: the on-disk index still contains it.
		if got := countMatches("original"); got != 0 {
			t.Errorf("the replaced content still matches %d times — the overlay is not shadowing the reader", got)
		}
	})

	t.Run("a deleted file stops matching", func(t *testing.T) {
		if err := os.Remove(filepath.Join(root, "b.go")); err != nil {
			t.Fatal(err)
		}
		waitForCount(t, func() int { return countMatches("appeared") }, 0, "the deleted file to stop matching")
	})

	t.Run("status reports the overlay", func(t *testing.T) {
		res := result(t, call(t, srv.Addr(), map[string]any{"jsonrpc": "2.0", "method": "status", "id": 1}))
		if !res["watcher_active"].(bool) {
			t.Error("watcher_active is false on a watching server")
		}
		if res["live_files"].(float64) == 0 {
			t.Error("live_files is zero after several edits")
		}
	})
}

// TestServerWithoutAWatcherStaysStatic is the control for the test above: it
// would pass trivially if the searches were somehow reading the filesystem
// directly, so this confirms that without a watcher the results do NOT change.
func TestServerWithoutAWatcherStaysStatic(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("func original() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := index.BuildFromPaths(dir, root, []string{"a.go"}, index.Options{Complete: true}); err != nil {
		t.Fatal(err)
	}
	idx, err := reader.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	srv := server.New(root, dir, idx) // deliberately no Watch
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Shutdown(); idx.Close() })

	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("func appeared() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	res := result(t, call(t, srv.Addr(), map[string]any{
		"jsonrpc": "2.0", "method": "search", "id": 1,
		"params": map[string]any{"pattern": "appeared"},
	}))
	if got := len(res["matches"].([]any)); got != 0 {
		t.Errorf("a server with no watcher reported %d matches for a file created after it started", got)
	}
	if res2 := result(t, call(t, srv.Addr(), map[string]any{"jsonrpc": "2.0", "method": "status", "id": 2})); res2["watcher_active"].(bool) {
		t.Error("watcher_active is true on a server started without one")
	}
}

// waitForCount polls until a count reaches want.
func waitForCount(t *testing.T, get func() int, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	last := -1
	for time.Now().Before(deadline) {
		if last = get(); last == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: last saw %d, want %d", what, last, want)
}
