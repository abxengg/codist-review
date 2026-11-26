package activity

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"time"
)

// A model call's id restarts at 1 in every run, so ids are prefixed with the run's start, and calls saved by earlier
// runs never clash with new ones.
var run = strconv.FormatInt(time.Now().UnixMilli(), 36)

var kept struct {
	path  string
	lines int
}

// Keep saves every finished model call to path, one JSON line each, and loads the calls saved there before, so the
// Activity page and "What the AI just did" keep their history when Codist restarts. Call it once, at start.
func Keep(path string) {
	var loaded []Call
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20) // a call carries its prompt and reply
		for sc.Scan() {
			var c Call
			if json.Unmarshal(sc.Bytes(), &c) == nil && c.ID != "" && c.Status != "running" {
				loaded = append(loaded, c)
			}
		}
		f.Close()
	}
	if len(loaded) > keepCalls {
		loaded = loaded[len(loaded)-keepCalls:]
	}
	calls.mu.Lock()
	calls.list = append(loaded, calls.list...)
	if len(calls.list) > keepCalls {
		calls.list = calls.list[len(calls.list)-keepCalls:]
	}
	kept.path = path
	rewrite()
	calls.mu.Unlock()
	slog.Debug("model calls loaded", "lib", "activity", "path", path, "calls", len(loaded))
}

// save adds one finished call to the file. The caller holds calls.mu.
func save(c Call) {
	if kept.path == "" {
		return
	}
	if kept.lines >= 2*keepCalls { // the file holds at most twice what is kept; then it is written afresh
		rewrite()
	}
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	f, err := os.OpenFile(kept.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("model call not saved", "lib", "activity", "err", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err == nil {
		kept.lines++
	}
}

// rewrite writes the file afresh with the finished calls kept in memory. The caller holds calls.mu.
func rewrite() {
	var buf bytes.Buffer
	n := 0
	for _, c := range calls.list {
		if c.Status == "running" {
			continue
		}
		if b, err := json.Marshal(c); err == nil {
			buf.Write(append(b, '\n'))
			n++
		}
	}
	tmp := kept.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		slog.Warn("model calls not saved", "lib", "activity", "err", err)
		return
	}
	if err := os.Rename(tmp, kept.path); err != nil {
		slog.Warn("model calls not saved", "lib", "activity", "err", err)
		return
	}
	kept.lines = n
}
