package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gregor-tokarev/fam/internal/transport"
)

func isPOSIXShell(shellPath string) bool {
	switch path.Base(strings.TrimSpace(shellPath)) {
	case "sh", "bash", "zsh", "dash", "ash", "ksh":
		return true
	}
	return false
}

func verifyMarker() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "__FAM_V_" + hex.EncodeToString(b[:])
}

// buildVerifyBatchScript runs all specs sequentially as isolated subshells inside ONE real login
// shell (either non-interactive or interactive PTY) started directly by sshd/local shell.
// Because sshd invokes the user's shell once and each check runs in `( eval ... )`:
//   - SHLVL stays 1 and startup files (.bashrc / .zshenv / .profile) run exactly once
//   - cd, exports, set flags, traps, exit codes (including 255), and syntax errors are isolated
//   - interactive PTY (SSH_TTY, -i) is preserved without spawning nested shells
//   - zero shell variables or temporary files are created
func buildVerifyBatchScript(specs []VerifySpec, marker string) string {
	var b strings.Builder
	for i, v := range specs {
		q := transport.Quote(v.Script)
		fmt.Fprintf(&b, "printf '%%s\\n' '%s_B %d'\n", marker, i)
		fmt.Fprintf(&b, "if ( eval %s ) 2>&1; then printf '\\n%%s 0\\n' '%s_E %d'; else printf '\\n%%s %%d\\n' '%s_E %d' \"$?\"; fi\n",
			q, marker, i, marker, i)
	}
	return b.String()
}

type batchVerifyItem struct {
	ran  bool
	code int
	out  string
}

func parseVerifyBatchOutput(raw string, marker string, count int) []batchVerifyItem {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	items := make([]batchVerifyItem, count)
	pos := 0
	for i := 0; i < count; i++ {
		beginTag := fmt.Sprintf("%s_B %d\n", marker, i)
		bi := strings.Index(raw[pos:], beginTag)
		if bi < 0 {
			break
		}
		bodyStart := pos + bi + len(beginTag)
		endPrefix := fmt.Sprintf("%s_E %d ", marker, i)
		ei := strings.Index(raw[bodyStart:], endPrefix)
		if ei < 0 {
			break
		}
		body := strings.TrimSuffix(raw[bodyStart:bodyStart+ei], "\n")
		codeStart := bodyStart + ei + len(endPrefix)
		nl := strings.IndexByte(raw[codeStart:], '\n')
		var codeStr string
		if nl < 0 {
			codeStr = strings.TrimSpace(raw[codeStart:])
			pos = len(raw)
		} else {
			codeStr = strings.TrimSpace(raw[codeStart : codeStart+nl])
			pos = codeStart + nl + 1
		}
		code, err := strconv.Atoi(codeStr)
		if err != nil {
			break
		}
		items[i] = batchVerifyItem{ran: true, code: code, out: body}
	}
	return items
}

// runVerifyBatch runs a slice of VerifySpecs in one login shell (interactive or non-interactive).
// Returns per-item shellRun results (index aligned with specs) and true if the batch protocol ran.
// Connection errors (transport exit 255 or script exit 255) are mapped to *transport.ConnError.
func runVerifyBatch(ctx context.Context, env *Env, specs []VerifySpec, interactive bool) ([]shellRun, bool) {
	if len(specs) < 2 || !isPOSIXShell(env.Facts.Shell) {
		return nil, false
	}
	marker := verifyMarker()
	script := buildVerifyBatchScript(specs, marker)
	// Avoid exceeding conservative command-line limits (e.g. macOS ARG_MAX).
	if len(script) > 96*1024 {
		return nil, false
	}
	shellLabel := verifyShells[0]
	if interactive {
		shellLabel = verifyShells[1]
	}
	timeout := time.Duration(len(specs)) * 5 * time.Minute
	if timeout > 30*time.Minute {
		timeout = 30 * time.Minute
	}
	res, err := env.T.Run(ctx, transport.Cmd{
		Script:      script,
		LoginShell:  true,
		Interactive: interactive,
		Timeout:     timeout,
		Label:       fmt.Sprintf("verify batch (%d %s)", len(specs), shellLabel),
	})

	combined := string(res.Stdout)
	if len(res.Stderr) > 0 {
		combined += "\n" + string(res.Stderr)
	}
	parsed := parseVerifyBatchOutput(combined, marker, len(specs))

	// If the batch shell itself failed before executing even the first marker and it was NOT a
	// connection error (e.g. an exotic user rc file broke eval), fall back to per-script runs.
	if !parsed[0].ran && !transport.IsConnError(err) {
		return nil, false
	}

	out := make([]shellRun, len(specs))
	for i, v := range specs {
		it := parsed[i]
		if !it.ran {
			// Transport disconnected or timed out before this script finished.
			if transport.IsConnError(err) {
				out[i] = shellRun{err: err, ran: true}
			} else if err != nil {
				out[i] = shellRun{row: verifyRow(v, shellLabel, res, err), ran: true}
			}
			// Stop at the first unfinished item after connection loss, matching serial reporting.
			break
		}
		if it.code == 255 {
			ce := &transport.ConnError{Machine: env.Machine.Name, Target: env.Machine.Target(), Stderr: it.out}
			out[i] = shellRun{err: ce, ran: true}
			break
		}
		r := transport.Result{Stdout: []byte(it.out), Code: it.code}
		out[i] = shellRun{row: verifyRow(v, shellLabel, r, nil), ran: true}
	}
	return out, true
}
