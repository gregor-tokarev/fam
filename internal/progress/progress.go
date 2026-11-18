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
	"unicode/utf8"
)

// slowTask is the time after which the plain mode writes a line for the current task.
const slowTask = 5 * time.Second

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Reporter writes the progress of a run.
type Reporter struct {
	w     io.Writer
	live  bool
	width func() int

	mu       sync.Mutex
	step     string
	stepAt   time.Time
	machines []*Machine
	drawn    int
	frame    int
	paused   bool
	stopped  bool
	stop     chan struct{}
	done     chan struct{}
}

// Machine is the progress of one machine.
type Machine struct {
	r       *Reporter
	name    string
	start   time.Time
	stage   string
	total   int
	index   int
	task    string
	taskAt  time.Time
	logged  bool
	status  string
	elapsed time.Duration
}

// New starts a reporter. live draws live lines; width returns the terminal width (it can be nil).
func New(w io.Writer, live bool, width func() int) *Reporter {
	r := &Reporter{w: w, live: live, width: width, stop: make(chan struct{}), done: make(chan struct{})}
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

// Pause removes the live lines until the returned function runs, for example for a password prompt.
func (r *Reporter) Pause() func() {
	if r == nil {
		return func() {}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clear()
	r.paused = true
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.paused = false
		r.draw()
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
	n, err := r.w.Write(p)
	r.draw()
	return n, err
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
		fmt.Fprintf(r.w, "fam: %s\n", msg)
	}
	r.draw()
}

// Machine adds a machine.
func (r *Reporter) Machine(name string) *Machine {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m := &Machine{r: r, name: name, start: time.Now()}
	r.machines = append(r.machines, m)
	return m
}

// Stage shows the current stage of the machine, for example "collecting facts".
func (m *Machine) Stage(stage string) {
	if m == nil {
		return
	}
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	m.stage, m.total, m.index, m.task, m.logged = stage, 0, 0, "", false
	if !m.r.live {
		fmt.Fprintf(m.r.w, "fam: %s: %s\n", m.name, stage)
	}
	m.r.draw()
}

// Count starts a stage that has total steps, for example "applying" with 40 tasks.
func (m *Machine) Count(stage string, total int) {
	if m == nil {
		return
	}
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	m.stage, m.total, m.index, m.task, m.logged = stage, total, 0, "", false
	if !m.r.live {
		fmt.Fprintf(m.r.w, "fam: %s: %s %d %s\n", m.name, stage, total, plural(total, "task", "tasks"))
	}
	m.r.draw()
}

// Task shows that step i (from 1) of the current stage starts.
func (m *Machine) Task(i int, label string) {
	if m == nil {
		return
	}
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	m.index, m.task, m.taskAt, m.logged = i, label, time.Now(), false
	m.r.draw()
}

// Done shows the result of the machine.
func (m *Machine) Done(status string) {
	if m == nil {
		return
	}
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	m.status = status
	m.elapsed = time.Since(m.start)
	if !m.r.live {
		fmt.Fprintf(m.r.w, "fam: %s: %s (%s)\n", m.name, status, round(m.elapsed))
	}
	m.r.draw()
}

// logSlow writes a plain line for each task that runs for a long time. The caller holds the lock.
func (r *Reporter) logSlow() {
	for _, m := range r.machines {
		if m.status != "" || m.task == "" || m.logged || time.Since(m.taskAt) < slowTask {
			continue
		}
		m.logged = true
		fmt.Fprintf(r.w, "fam: %s: %s\n", m.name, m.progress())
	}
}

// progress is the stage, the counter, and the task of the machine.
func (m *Machine) progress() string {
	s := m.stage
	if m.total > 0 {
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
	var b strings.Builder
	fmt.Fprintf(&b, "\r\x1b[%dA\x1b[J", r.drawn)
	io.WriteString(r.w, b.String())
	r.drawn = 0
}

// draw draws the live lines again. The caller holds the lock.
func (r *Reporter) draw() {
	if !r.live || r.paused || r.stopped {
		return
	}
	var lines []string
	spin := spinner[r.frame%len(spinner)]
	if len(r.machines) == 0 {
		if r.step == "" {
			return
		}
		lines = append(lines, fmt.Sprintf("%s %s%s", spin, r.step, since(r.stepAt)))
	}
	nameWidth := 0
	for _, m := range r.machines {
		nameWidth = max(nameWidth, utf8.RuneCountInString(m.name))
	}
	for _, m := range r.machines {
		var line string
		switch {
		case m.status != "":
			line = fmt.Sprintf("%s %-*s  %s (%s)", mark(m.status), nameWidth, m.name, m.status, round(m.elapsed))
		default:
			line = fmt.Sprintf("%s %-*s  %s", spin, nameWidth, m.name, m.progress())
			if m.task != "" {
				line += since(m.taskAt)
			} else {
				line += since(m.start)
			}
		}
		lines = append(lines, line)
	}
	width := 0
	if r.width != nil {
		width = r.width()
	}
	var b strings.Builder
	if r.drawn > 0 {
		fmt.Fprintf(&b, "\r\x1b[%dA", r.drawn)
	}
	for _, l := range lines {
		b.WriteString("\r\x1b[2K")
		b.WriteString(truncate(l, width-1))
		b.WriteString("\n")
	}
	b.WriteString("\x1b[J")
	io.WriteString(r.w, b.String())
	r.drawn = len(lines)
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

func truncate(s string, width int) string {
	if width <= 0 || utf8.RuneCountInString(s) <= width {
		return s
	}
	runes := []rune(s)
	return string(runes[:width-1]) + "…"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
