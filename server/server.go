// Package server serves searches over TCP so that an index is built once and
// queried instantly thereafter.
//
// The protocol is JSON-RPC 2.0 over newline-delimited TCP: one request object
// per line, one response object per line. It is deliberately the same wire
// format the Rust implementation speaks, so either server can answer either
// client — which is also what lets the two be diffed against each other.
//
// Four methods: search, files, status, reload.
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/notshekhar/tgrep-go/hybrid"
	"github.com/notshekhar/tgrep-go/live"
	"github.com/notshekhar/tgrep-go/matching"
	"github.com/notshekhar/tgrep-go/reader"
	"github.com/notshekhar/tgrep-go/search"
)

// Server answers searches against one root.
type Server struct {
	root     string
	indexDir string

	// index merges the on-disk reader with the live overlay, so a search sees
	// edits made since the index was built.
	index *hybrid.Index
	// watcher keeps that overlay in step with the filesystem. Nil when the
	// server was started without one.
	watcher *live.Watcher

	// mu guards the reader swap performed by reload. Searches do not take it —
	// the hybrid index has its own — but reload does, so two concurrent
	// reloads cannot both install a reader and leak one.
	mu sync.Mutex

	listener net.Listener
	// indexing reports a background build in progress, which `status` exposes
	// so a client can tell "no matches" from "not ready yet".
	indexing atomic.Bool
	searches atomic.Int64
	started  time.Time
}

// New returns a server over an already-built index, with no watcher: the
// overlay stays empty and results reflect the index as built.
func New(root, indexDir string, index *reader.Reader) *Server {
	return &Server{
		root:     root,
		indexDir: indexDir,
		index:    hybrid.NewWith(index, root),
		started:  time.Now(),
	}
}

// Watch starts a file watcher feeding this server's overlay, so edits become
// searchable without a rebuild.
//
// Returned rather than started by New so that a caller who wants a static
// index — a one-shot query, a test — does not pay for a watcher it will not
// use. Close stops it.
func (s *Server) Watch(opts live.WatchOptions) error {
	w, err := live.NewWatcher(s.root, s.index.Live, opts)
	if err != nil {
		return err
	}
	s.watcher = w
	return nil
}

// Addr is the address the server is listening on, valid after Listen.
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Listen binds the server. A port of 0 asks the OS to choose one, which is
// what the tests use to avoid fighting over a fixed port.
func (s *Server) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.listener = ln
	return s.writePortFile()
}

// portFile is where a client looks to find a running server for a root.
func (s *Server) portFile() string { return filepath.Join(s.indexDir, "server.port") }

func (s *Server) writePortFile() error {
	if err := os.MkdirAll(s.indexDir, 0o755); err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		return err
	}
	return os.WriteFile(s.portFile(), []byte(port), 0o644)
}

// Serve accepts connections until the listener is closed.
//
// Each connection is handled on its own goroutine and may issue any number of
// requests, so a client that keeps a socket open pays the connection cost once.
func (s *Server) Serve() error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			// A closed listener is how Shutdown stops this loop, not a fault.
			return nil
		}
		go s.handleConn(conn)
	}
}

// Shutdown stops accepting, stops the watcher, and removes the port file, so a
// later client does not try to reach a server that is gone.
func (s *Server) Shutdown() error {
	os.Remove(s.portFile())
	if s.watcher != nil {
		s.watcher.Close()
		s.watcher = nil
	}
	if s.listener == nil {
		return nil
	}
	return s.listener.Close()
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	// A request carrying many long patterns can exceed the default token size,
	// and a truncated request would parse as a different one.
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	writer := bufio.NewWriter(conn)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		response := s.dispatch(line)
		if _, err := writer.Write(response); err != nil {
			return
		}
		if err := writer.WriteByte('\n'); err != nil {
			return
		}
		if err := writer.Flush(); err != nil {
			return
		}
	}
}

// request is one JSON-RPC call. id is kept as raw JSON because the spec allows
// a string, a number or null, and echoing back exactly what arrived is simpler
// and more correct than modelling the union.
type request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	ID     json.RawMessage `json:"id"`
}

func (s *Server) dispatch(line []byte) []byte {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return errorResponse(nil, -32700, "Parse error: "+err.Error())
	}
	switch req.Method {
	case "search":
		return s.handleSearch(req)
	case "files":
		return s.handleFiles(req)
	case "status":
		return s.handleStatus(req)
	case "reload":
		return s.handleReload(req)
	default:
		return errorResponse(req.ID, -32601, "Method not found: "+req.Method)
	}
}

// searchParams is the request shape, matching the reference's parameter names
// so one client can drive either server.
type searchParams struct {
	Pattern         string   `json:"pattern"`
	Patterns        []string `json:"patterns"`
	CaseInsensitive bool     `json:"case_insensitive"`
	FixedString     bool     `json:"fixed_string"`
	WordBoundary    bool     `json:"word_boundary"`
	LineRegexp      bool     `json:"line_regexp"`
	Multiline       bool     `json:"multiline"`
	MultilineDotAll bool     `json:"multiline_dotall"`
	InvertMatch     bool     `json:"invert_match"`
	OnlyMatching    bool     `json:"only_matching"`
	BeforeContext   int      `json:"before_context"`
	AfterContext    int      `json:"after_context"`
	MaxCount        int      `json:"max_count"`
	MaxFileSize     int64    `json:"max_filesize"`
	Text            bool     `json:"text"`
	FilesOnly       bool     `json:"files_only"`
	// Detail asks for spans and columns, which are what make a reply large.
	Detail bool `json:"detail"`
}

// matchRow is one row of a search reply.
type matchRow struct {
	Type    string   `json:"type"`
	File    string   `json:"file"`
	Line    int      `json:"line"`
	Content string   `json:"content"`
	Spans   [][2]int `json:"spans,omitempty"`
	Columns []int    `json:"columns,omitempty"`
}

func (s *Server) handleSearch(req request) []byte {
	s.searches.Add(1)

	var p searchParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return errorResponse(req.ID, -32602, "Invalid params: "+err.Error())
		}
	}

	patterns := p.Patterns
	if p.Pattern != "" {
		patterns = append([]string{p.Pattern}, patterns...)
	}
	if len(patterns) == 0 {
		return errorResponse(req.ID, -32602, "Invalid params: no pattern given")
	}

	started := time.Now()
	searcher := search.New(s.root, s.index)
	res, err := searcher.Run(search.Options{
		Patterns: patterns,
		Matcher: matching.Config{
			FixedString:     p.FixedString,
			CaseInsensitive: p.CaseInsensitive,
			WordBoundary:    p.WordBoundary,
			LineRegexp:      p.LineRegexp,
			Multiline:       p.Multiline,
			MultilineDotAll: p.MultilineDotAll,
		},
		Match: matching.Options{
			InvertMatch:   p.InvertMatch,
			OnlyMatching:  p.OnlyMatching,
			BeforeContext: p.BeforeContext,
			AfterContext:  p.AfterContext,
			MaxCount:      p.MaxCount,
			AllSpans:      p.Detail,
		},
		MaxFileSize:  p.MaxFileSize,
		SearchBinary: p.Text,
	})
	if err != nil {
		return errorResponse(req.ID, -32602, err.Error())
	}

	rows := []matchRow{}
	for _, fr := range res.Files {
		if fr.Err != nil {
			continue
		}
		if p.FilesOnly {
			rows = append(rows, matchRow{Type: "file", File: fr.Path})
			continue
		}
		for _, line := range fr.Lines {
			row := matchRow{
				Type:    "match",
				File:    fr.Path,
				Line:    line.Number,
				Content: string(line.Text),
			}
			if line.Kind == matching.KindContext {
				row.Type = "context"
			}
			if p.Detail {
				for _, sp := range line.Spans {
					row.Spans = append(row.Spans, [2]int{sp.Start, sp.End})
				}
				row.Columns = line.Columns()
			}
			rows = append(rows, row)
		}
	}

	return resultResponse(req.ID, map[string]any{
		"matches":     rows,
		"num_matches": len(rows),
		"elapsed_ms":  time.Since(started).Milliseconds(),
	})
}

func (s *Server) handleFiles(req request) []byte {
	paths := s.index.AllPaths()
	sort.Strings(paths)
	return resultResponse(req.ID, map[string]any{"files": paths})
}

func (s *Server) handleStatus(req request) []byte {
	return resultResponse(req.ID, map[string]any{
		"num_files":      s.index.NumFiles(),
		"num_trigrams":   s.index.NumTrigrams(),
		"live_files":     s.index.LiveFiles(),
		"watcher_active": s.watcher != nil,
		"indexing":       s.indexing.Load(),
		"searches":       s.searches.Load(),
		"uptime_seconds": int64(time.Since(s.started).Seconds()),
		"root":           s.root,
		"index_dir":      s.indexDir,
	})
}

// handleReload reopens the index from disk, for use after an out-of-band
// rebuild.
//
// The old reader is closed only after the new one is in place: on Windows a
// mapped file cannot be replaced, and closing first would leave a window in
// which the server has no index at all.
func (s *Server) handleReload(req request) []byte {
	fresh, err := reader.Open(s.indexDir)
	if err != nil {
		return errorResponse(req.ID, -32002, "reload failed: "+err.Error())
	}

	s.mu.Lock()
	old := s.index.SwapReader(fresh)
	s.mu.Unlock()

	// The overlay is cleared only AFTER the new reader is installed: it exists
	// to cover what the old reader was missing, and dropping it first would
	// open a window in which neither layer knows about a recent edit.
	s.index.Live.Clear()

	if old != nil {
		old.Close()
	}
	return resultResponse(req.ID, map[string]any{
		"num_files":    s.index.NumFiles(),
		"num_trigrams": s.index.NumTrigrams(),
	})
}

func resultResponse(id json.RawMessage, result any) []byte {
	return encodeResponse(map[string]any{
		"jsonrpc": "2.0",
		"result":  result,
		"id":      rawOrNull(id),
	})
}

func errorResponse(id json.RawMessage, code int, message string) []byte {
	return encodeResponse(map[string]any{
		"jsonrpc": "2.0",
		"error":   map[string]any{"code": code, "message": message},
		"id":      rawOrNull(id),
	})
}

// rawOrNull echoes the request's id verbatim, or null when it had none.
func rawOrNull(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return id
}

func encodeResponse(v map[string]any) []byte {
	encoded, err := json.Marshal(v)
	if err != nil {
		// Falling back by hand keeps a serialisation failure from dropping the
		// response entirely, which would hang a client waiting for its reply.
		return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-32603,"message":%q},"id":null}`, err.Error()))
	}
	return encoded
}

// ReadPort returns the port a server for this index directory is listening on.
func ReadPort(indexDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(indexDir, "server.port"))
	if err != nil {
		return "", err
	}
	return string(data), nil
}
