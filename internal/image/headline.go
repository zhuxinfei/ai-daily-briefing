// Package image wraps the external headline image generator script
// (scripts/gen_headline.py) in a small Go-native interface. We keep
// the actual drawing in Python so we can lean on PIL's CJK-aware
// typography without reimplementing it; Go's role is limited to
// shelling out, bounding the runtime, and collecting the output path.
package image

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config describes how to invoke the Python headline generator. All
// paths are resolved lazily inside Render — callers can pass relative
// paths (e.g. the values baked into config/ai.yaml) and they will be
// interpreted against the current working directory at call time.
type Config struct {
	// PythonBin is the interpreter to run (e.g. "python3"). Defaults
	// to "python3" when left empty.
	PythonBin string
	// ScriptPath is the path to gen_headline.py.
	ScriptPath string
	// OutputDir is the directory where generated PNGs are written. It
	// is created (recursively) if it does not exist.
	OutputDir string
	// Width and Height control the output canvas size. When either is
	// zero, the Python script's own defaults are used (1200x630).
	Width  int
	Height int
	// FontBold / FontRegular are the TTF/TTC paths handed to the
	// script. Both must point at a CJK-capable font or Chinese
	// headlines will render as tofu boxes.
	FontBold    string
	FontRegular string
	// Timeout bounds the subprocess runtime. Zero uses a 30s default.
	Timeout time.Duration
}

// Renderer is a reusable headline generator wired to one Config.
type Renderer struct {
	cfg Config
}

// New returns a Renderer bound to cfg. The Config is copied so later
// mutations by the caller do not affect this instance.
func New(cfg Config) *Renderer {
	if cfg.PythonBin == "" {
		cfg.PythonBin = "python3"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &Renderer{cfg: cfg}
}

// Render generates a headline PNG for date with the given headline +
// subtitle text. On success it returns the absolute path to the
// generated PNG. On failure it returns an error that wraps the
// script's stderr (trimmed to a reasonable length) so callers can log
// something actionable without dumping megabytes.
//
// The context is honoured: if ctx is cancelled before the subprocess
// exits, the subprocess is killed and ctx.Err() is returned.
func (r *Renderer) Render(ctx context.Context, date time.Time, headline, subtitle string) (string, error) {
	if strings.TrimSpace(r.cfg.ScriptPath) == "" {
		return "", fmt.Errorf("image: ScriptPath is empty")
	}
	if strings.TrimSpace(r.cfg.OutputDir) == "" {
		return "", fmt.Errorf("image: OutputDir is empty")
	}
	if strings.TrimSpace(headline) == "" {
		return "", fmt.Errorf("image: headline is empty")
	}

	// Resolve paths to absolute form so the returned path is stable
	// no matter what cwd the caller was in.
	absOutputDir, err := filepath.Abs(r.cfg.OutputDir)
	if err != nil {
		return "", fmt.Errorf("image: resolve output dir: %w", err)
	}
	if err := os.MkdirAll(absOutputDir, 0o755); err != nil {
		return "", fmt.Errorf("image: mkdir %s: %w", absOutputDir, err)
	}

	absScriptPath, err := filepath.Abs(r.cfg.ScriptPath)
	if err != nil {
		return "", fmt.Errorf("image: resolve script path: %w", err)
	}

	outputPath := filepath.Join(absOutputDir, date.Format("2006-01-02")+".png")

	subCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	args := []string{
		absScriptPath,
		"--date", date.Format("2006-01-02"),
		"--headline", headline,
		"--subtitle", subtitle,
		"--output", outputPath,
	}
	if r.cfg.Width > 0 {
		args = append(args, "--width", strconv.Itoa(r.cfg.Width))
	}
	if r.cfg.Height > 0 {
		args = append(args, "--height", strconv.Itoa(r.cfg.Height))
	}
	if strings.TrimSpace(r.cfg.FontBold) != "" {
		args = append(args, "--font-bold", r.cfg.FontBold)
	}
	if strings.TrimSpace(r.cfg.FontRegular) != "" {
		args = append(args, "--font-regular", r.cfg.FontRegular)
	}

	cmd := exec.CommandContext(subCtx, r.cfg.PythonBin, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Surface context cancellation / timeout directly so callers
		// can distinguish it from a Python-side failure.
		if ctxErr := subCtx.Err(); ctxErr != nil {
			return "", fmt.Errorf("image: subprocess cancelled: %w (stderr: %s)", ctxErr, truncate(stderr.String(), 500))
		}
		return "", fmt.Errorf("image: %s %s failed: %w (stderr: %s)",
			r.cfg.PythonBin, filepath.Base(absScriptPath), err, truncate(stderr.String(), 500))
	}

	// Sanity check: the script is expected to print "OK: <path>" on
	// success. We don't parse it strictly, but if the file doesn't
	// exist that's a hard failure.
	if fi, statErr := os.Stat(outputPath); statErr != nil || fi.Size() == 0 {
		return "", fmt.Errorf("image: output missing or empty after script ran (stdout: %s; stderr: %s)",
			truncate(stdout.String(), 500), truncate(stderr.String(), 500))
	}

	return outputPath, nil
}

// truncate clips s to at most max characters (on rune boundaries) and
// appends an ellipsis when it had to cut. Used to keep error messages
// bounded so they don't spam logs.
func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}
