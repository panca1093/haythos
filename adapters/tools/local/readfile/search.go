package readfile

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/panca/haythos/core/task"
)

const (
	searchTimeout   = 10 * time.Second
	maxSearchOutput = 16 << 10
	moreResults     = "[more results; narrow path or glob]"
)

var searchSpec = task.ToolSpec{
	Name:        "file.search",
	Description: "Search file contents with ripgrep. Start here before file.read: mode=files (default) lists matching files, content shows matching lines with line numbers, count counts per file. Narrow with path and glob.",
	InputSchema: json.RawMessage(pathArg + `,"pattern":{"type":"string","description":"regular expression"},"glob":{"type":"string","description":"e.g. *.go"},"mode":{"type":"string","enum":["files","content","count"]},"limit":{"type":"integer","minimum":1}},"required":["pattern"]}`),
}

func (t Guard) search(ctx context.Context, raw []byte) (task.ToolResult, error) {
	var a struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Glob    string `json:"glob"`
		Mode    string `json:"mode"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Pattern == "" || a.Limit < 0 {
		return refuse("pattern is required; limit must be positive")
	}
	flag, limit := "-l", 100
	switch a.Mode {
	case "", "files":
	case "content":
		flag, limit = "-n", 50
	case "count":
		flag = "-c"
	default:
		return refuse("mode must be files, content or count")
	}
	if a.Limit > 0 {
		limit = a.Limit
	}
	target, why := t.Resolve(ctx, a.Path)
	if why != "" {
		return refuse(why)
	}
	argv := []string{t.cfg.Rg, "--no-config", "--color", "never", "--max-columns", "300", "--max-columns-preview", "--sort", "path", "--with-filename", "--no-heading", flag, "-e", a.Pattern}
	if argv[0] == "" {
		argv[0] = "rg"
	}
	if flag != "-l" {
		argv = append(argv, "--null")
	}
	if a.Glob != "" {
		argv = append(argv, "-g", a.Glob)
	}
	argv = append(argv, "--", target.Rel)

	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	out, over, err := Run(ctx, target.Root, argv, nil, maxSearchOutput)
	if ctx.Err() != nil {
		return refuse("file.search timed out; narrow path or glob")
	}
	if err != nil {
		var exit *exec.ExitError
		switch {
		case errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist):
			return refuse("file.search needs ripgrep (rg) installed")
		case errors.As(err, &exit) && exit.ExitCode() == 1:
		case errors.As(err, &exit) && exit.ExitCode() == 2 && out != "":
		default:
			return refuse(err.Error())
		}
	}
	if over {
		if i := strings.LastIndexByte(out, '\n'); i >= 0 {
			out = out[:i+1]
		} else {
			out = ""
		}
	}

	var lines []string
	untrusted := false
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		file, rest, hasRest := strings.Cut(line, "\x00")
		file = filepath.Clean(file)
		if t.Denied(filepath.Join(target.Root, file)) {
			continue
		}
		if a.Mode == "content" && !IsSource(file) {
			untrusted = true
		}
		if hasRest && rest != "" {
			file += ":" + rest
		}
		lines = append(lines, file)
	}
	if len(lines) > limit {
		lines, over = lines[:limit], true
	}
	if len(lines) == 0 {
		return task.ToolResult{Content: "No matches."}, nil
	}
	text := strings.Join(lines, "\n")
	if over {
		text += "\n" + moreResults
	}
	return task.ToolResult{Content: task.StripInvisible(text), Untrusted: untrusted}, nil
}
