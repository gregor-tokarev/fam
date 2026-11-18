// Package progress shows what a run does while it runs.
//
// On a terminal, the reporter draws one live line for each machine and redraws it some times
// each second. In other cases it writes plain lines: one line when a stage starts, and one line
// for each task that runs for a long time. All methods are safe on a nil reporter.
package progress

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/width"

	"github.com/gregor-tokarev/fam/internal/output"
)

// slowTask is the time after which the plain mode writes a line for the current task.
const slowTask = 5 * time.Second

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Size returns the width and the height of the terminal. A value of 0 means unknown.
type Size func() (cols, rows int)

// Reporter writes the progress of a run.
type Reporter struct {
	w    io.Writer
	live bool
	size Size

	mu       sync.Mutex
	redactor *output.Redactor
	step     string
	stepAt   time.Time
	machines []*Machine
	drawn    int
	frame    int
	// paused counts the pauses that did not end yet: checks that run at the same time can wait
	// for the same password prompt.
	paused  int
	stopped bool
	stop    chan struct{}
	done    chan struct{}
}

// Machine is the progress of one machine.
type Machine struct {
	r     *Reporter
	name  string
	start time.Time
	// stage is the current stage. count and total are the stage with a counter, for example
	// "applying" with 40 tasks. A short stage (for example a restart) replaces stage until the
	// next task.
	stage   string
	count   string
	total   int
	index   int
	task    string
	taskAt  time.Time
	logged  bool
	status  string
	elapsed time.Duration
}

// New starts a reporter. live draws live lines; size returns the terminal size (it can be nil).
func New(w io.Writer, live bool, size Size) *Reporter {
	r := &Reporter{w: w, live: live, size: size, stop: make(chan struct{}), done: make(chan struct{})}
	go r.loop()
	return r
}

func (r *Reporter) loop() {
	defer close(r.done)
	interval := time.Second
	if r.live {
		interval = 100 * time.Millisecond
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-tick.C:
			r.mu.Lock()
			if r.live {
				r.frame++
				r.draw()
			} else {
				r.logSlow()
			}
			r.mu.Unlock()
		}
	}
}

// Stop ends the reporter and removes the live lines.
func (r *Reporter) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	r.mu.Unlock()
	close(r.stop)
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clear()
}

// SetSecrets sets the secret values that the progress lines hide.
func (r *Reporter) SetSecrets(values []string) {
	if r == nil {
		return
	}
	rd := output.NewRedactor(values)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.redactor = rd
}

// Pause removes the live lines until the returned function runs, for example for a password prompt.
func (r *Reporter) Pause() func() {
	if r == nil {
		return func() {}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clear()
	r.paused++
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.paused--
		})
	}
}

// Write writes a message above the live lines. Other packages can use the reporter as an io.Writer.
func (r *Reporter) Write(p []byte) (int, error) {
	if r == nil {
		return len(p), nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clear()
	return r.w.Write(p)
}

// Step shows a step of the run that is not for one machine.
func (r *Reporter) Step(msg string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.step, r.stepAt = msg, time.Now()
	if !r.live {
		r.plain(msg)
	}
}

// Machine adds a machine. Its timer starts with its first stage.
func (r *Reporter) Machine(name string) *Machine {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m := &Machine{r: r, name: name}
	r.machines = append(r.machines, m)
	return m
}

// Stage shows the current stage of the machine, for example "collecting facts".
// The next Task returns to the stage of the last Count.
func (m *Machine) Stage(stage string) {
	if m == nil {
		return
	}
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	m.begin()
	m.stage, m.task, m.logged = stage, "", false
	if !m.r.live {
		m.r.plain(m.name + ": " + stage)
	}
}

// Count starts a stage that has total steps, for example "applying" with 40 tasks.
func (m *Machine) Count(stage string, total int) {
	if m == nil {
		return
	}
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	m.begin()
	m.stage, m.count, m.total, m.index, m.task, m.logged = stage, stage, total, 0, "", false
	if !m.r.live {
		m.r.plain(fmt.Sprintf("%s: %s %d %s", m.name, stage, total, plural(total, "task", "tasks")))
	}
}

// Task shows that step i (from 1) of the stage of the last Count starts.
func (m *Machine) Task(i int, label string) {
	if m == nil {
		return
	}
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	m.begin()
	m.stage, m.index, m.task, m.taskAt, m.logged = m.count, i, label, time.Now(), false
}

// Done shows the result of the machine.
func (m *Machine) Done(status string) {
	if m == nil {
		return
	}
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	m.begin()
	m.status = status
	m.elapsed = time.Since(m.start)
	if !m.r.live {
		m.r.plain(fmt.Sprintf("%s: %s (%s)", m.name, status, round(m.elapsed)))
	}
}

// begin starts the timer of the machine. The caller holds the lock.
func (m *Machine) begin() {
	if m.start.IsZero() {
		m.start = time.Now()
	}
}

// plain writes one plain line. The caller holds the lock.
func (r *Reporter) plain(msg string) {
	fmt.Fprintf(r.w, "fam: %s\n", r.redactor.String(msg))
}

// logSlow writes a plain line for each task that runs for a long time. The caller holds the lock.
func (r *Reporter) logSlow() {
	for _, m := range r.machines {
		if m.status != "" || m.task == "" || m.logged || time.Since(m.taskAt) < slowTask {
			continue
		}
		m.logged = true
		r.plain(m.name + ": " + m.progress())
	}
}

// progress is the stage, the counter, and the task of the machine.
func (m *Machine) progress() string {
	s := m.stage
	// Steps that run at the same time report only the count, without a task name.
	if m.total > 0 && m.stage == m.count && (m.task != "" || m.index > 0) {
		s += fmt.Sprintf(" %d/%d", m.index, m.total)
	}
	if m.task != "" {
		s += ": " + m.task
	}
	return s
}

// clear removes the live lines. The caller holds the lock.
func (r *Reporter) clear() {
	if !r.live || r.drawn == 0 {
		return
	}
	fmt.Fprintf(r.w, "\r\x1b[%dA\x1b[J", r.drawn)
	r.drawn = 0
}

// draw draws the live lines again. Only the timer calls it, so that the number of writes does
// not grow with the number of tasks. The caller holds the lock.
func (r *Reporter) draw() {
	if !r.live || r.paused > 0 || r.stopped {
		return
	}
	cols, rows := 0, 0
	if r.size != nil {
		cols, rows = r.size()
	}
	spin := spinner[r.frame%len(spinner)]
	var lines []string
	if len(r.machines) == 0 {
		if r.step == "" {
			return
		}
		lines = append(lines, fmt.Sprintf("%s %s%s", spin, r.step, since(r.stepAt)))
	}
	// The live lines must fit on the screen: the cursor cannot go up into the scrollback,
	// so lines that scroll out of the screen cannot be cleared.
	shown := r.machines
	maxLines := rows - 1
	if rows > 0 && len(shown) > maxLines {
		shown = r.visible(max(maxLines-1, 0))
	}
	nameWidth := 0
	for _, m := range shown {
		nameWidth = max(nameWidth, width1(m.name))
	}
	for _, m := range shown {
		pad := strings.Repeat(" ", nameWidth-width1(m.name))
		var line string
		switch {
		case m.status != "":
			line = fmt.Sprintf("%s %s%s  %s (%s)", mark(m.status), m.name, pad, m.status, round(m.elapsed))
		case m.start.IsZero():
			line = fmt.Sprintf("  %s%s  waiting", m.name, pad)
		default:
			line = fmt.Sprintf("%s %s%s  %s", spin, m.name, pad, m.progress())
			if m.task != "" {
				line += since(m.taskAt)
			} else {
				line += since(m.start)
			}
		}
		lines = append(lines, line)
	}
	if hidden := len(r.machines) - len(shown); hidden > 0 && maxLines > 0 {
		done := 0
		for _, m := range r.machines {
			if m.status != "" {
				done++
			}
		}
		lines = append(lines, fmt.Sprintf("  … %d more %s (%d of %d done)", hidden, plural(hidden, "machine", "machines"), done, len(r.machines)))
	}
	var b strings.Builder
	if r.drawn > 0 {
		fmt.Fprintf(&b, "\r\x1b[%dA", r.drawn)
	}
	for _, l := range lines {
		b.WriteString("\r\x1b[2K")
		b.WriteString(truncate(r.redactor.String(l), cols-1))
		b.WriteString("\n")
	}
	b.WriteString("\x1b[J")
	io.WriteString(r.w, b.String())
	r.drawn = len(lines)
}

// visible picks n machines for a small screen: running machines first, then waiting, then done.
// They keep their order. The caller holds the lock.
func (r *Reporter) visible(n int) []*Machine {
	pick := map[*Machine]bool{}
	for _, want := range []func(*Machine) bool{
		func(m *Machine) bool { return m.status == "" && !m.start.IsZero() },
		func(m *Machine) bool { return m.status == "" },
		func(m *Machine) bool { return true },
	} {
		for _, m := range r.machines {
			if len(pick) >= n {
				break
			}
			if want(m) {
				pick[m] = true
			}
		}
	}
	var out []*Machine
	for _, m := range r.machines {
		if pick[m] {
			out = append(out, m)
		}
	}
	return out
}

func mark(status string) string {
	switch status {
	case "ok", "changes", "changed":
		return "✓"
	case "conflict":
		return "!"
	}
	return "✗"
}

// since shows the time after start when it is 2 seconds or more.
func since(start time.Time) string {
	d := time.Since(start)
	if d < 2*time.Second {
		return ""
	}
	return fmt.Sprintf(" (%s)", d.Truncate(time.Second))
}

func round(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(100 * time.Millisecond)
}

// cells is the number of terminal columns of a rune.
func cells(r rune) int {
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}

// width1 is the number of terminal columns of s.
func width1(s string) int {
	n := 0
	for _, r := range s {
		n += cells(r)
	}
	return n
}

// truncate cuts s to at most w terminal columns. Control characters become spaces, so that
// a label cannot move the cursor.
func truncate(s string, w int) string {
	var b strings.Builder
	n := 0
	limit := w
	if w > 0 && width1(s) > w {
		limit = w - 1
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		c := cells(r)
		if w > 0 && n+c > limit {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n += c
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
