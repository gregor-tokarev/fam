// Package output holds the result of a run and writes it as text, JSON, or TOON.
package output

import (
	"github.com/gregor-tokarev/fam/internal/facts"
)

// Run is the result of one `fam plan`, `fam apply`, or `fam verify`.
// Text, JSON, and TOON output use this one structure.
type Run struct {
	Command  string          `json:"command" toon:"command"`
	RunID    string          `json:"runId" toon:"runId"`
	Version  string          `json:"version" toon:"version"`
	Started  string          `json:"started" toon:"started"`
	Duration string          `json:"duration" toon:"duration"`
	Base     string          `json:"base" toon:"base"`
	Config   string          `json:"config" toon:"config"`
	OK       bool            `json:"ok" toon:"ok"`
	Error    string          `json:"error,omitempty" toon:"error,omitempty"`
	Machines []MachineResult `json:"machines" toon:"machines"`
	Summary  Counts          `json:"summary" toon:"summary"`
	LogFile  string          `json:"logFile,omitempty" toon:"logFile,omitempty"`
}

// MachineResult is the result for one machine.
type MachineResult struct {
	Name      string `json:"name" toon:"name"`
	Role      string `json:"role" toon:"role"`
	Target    string `json:"target,omitempty" toon:"target,omitempty"`
	Transport string `json:"transport" toon:"transport"`
	// Status is "ok", "changes", "changed", "failed", "conflict", or "error".
	Status string `json:"status" toon:"status"`
	// ErrorKind is "connection", "unsupported", "config", "sudo", or "task" when Status is "error" or "failed".
	ErrorKind string          `json:"errorKind,omitempty" toon:"errorKind,omitempty"`
	Error     string          `json:"error,omitempty" toon:"error,omitempty"`
	Facts     *facts.Facts    `json:"facts,omitempty" toon:"facts,omitempty"`
	Tasks     []TaskResult    `json:"tasks" toon:"tasks"`
	Restarts  []RestartResult `json:"restarts,omitempty" toon:"restarts,omitempty"`
	Verify    []VerifyResult  `json:"verify,omitempty" toon:"verify,omitempty"`
	Counts    Counts          `json:"counts" toon:"counts"`
}

// TaskResult is the result of one task.
type TaskResult struct {
	Group string `json:"group" toon:"group"`
	Kind  string `json:"kind" toon:"kind"`
	Name  string `json:"name" toon:"name"`
	// Status: plan gives "satisfied", "change", "conflict", "error".
	// Apply gives "unchanged", "changed", "failed", "conflict", "skipped".
	Status string `json:"status" toon:"status"`
	Detail string `json:"detail,omitempty" toon:"detail,omitempty"`
	Error  string `json:"error,omitempty" toon:"error,omitempty"`
}

// RestartResult is the restart of one unit.
type RestartResult struct {
	Unit   string `json:"unit" toon:"unit"`
	Scope  string `json:"scope" toon:"scope"`
	Status string `json:"status" toon:"status"`
	Error  string `json:"error,omitempty" toon:"error,omitempty"`
}

// VerifyResult is the result of one verify script in one shell.
type VerifyResult struct {
	Bundle string `json:"bundle" toon:"bundle"`
	Name   string `json:"name" toon:"name"`
	Event  string `json:"event" toon:"event"`
	Shell  string `json:"shell" toon:"shell"`
	Status string `json:"status" toon:"status"`
	Code   int    `json:"code" toon:"code"`
	Output string `json:"output,omitempty" toon:"output,omitempty"`
}

// Counts are the number of tasks for each status.
type Counts struct {
	Satisfied int `json:"satisfied" toon:"satisfied"`
	Change    int `json:"change" toon:"change"`
	Unchanged int `json:"unchanged" toon:"unchanged"`
	Changed   int `json:"changed" toon:"changed"`
	Failed    int `json:"failed" toon:"failed"`
	Error     int `json:"error" toon:"error"`
	Conflict  int `json:"conflict" toon:"conflict"`
	Skipped   int `json:"skipped" toon:"skipped"`
	VerifyOK  int `json:"verifyPassed" toon:"verifyPassed"`
	VerifyBad int `json:"verifyFailed" toon:"verifyFailed"`
}

// Add counts one task status.
func (c *Counts) Add(status string) { c.add(status, 1) }

// Move counts a task under `to` instead of `from`.
func (c *Counts) Move(from, to string) {
	c.add(from, -1)
	c.add(to, 1)
}

func (c *Counts) add(status string, n int) {
	switch status {
	case "satisfied":
		c.Satisfied += n
	case "change":
		c.Change += n
	case "unchanged":
		c.Unchanged += n
	case "changed":
		c.Changed += n
	case "failed":
		c.Failed += n
	case "error":
		c.Error += n
	case "conflict":
		c.Conflict += n
	case "skipped":
		c.Skipped += n
	}
}

// Plus adds other to c.
func (c *Counts) Plus(o Counts) {
	c.Satisfied += o.Satisfied
	c.Change += o.Change
	c.Unchanged += o.Unchanged
	c.Changed += o.Changed
	c.Failed += o.Failed
	c.Error += o.Error
	c.Conflict += o.Conflict
	c.Skipped += o.Skipped
	c.VerifyOK += o.VerifyOK
	c.VerifyBad += o.VerifyBad
}
