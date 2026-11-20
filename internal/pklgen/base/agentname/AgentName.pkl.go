// Code generated from Pkl module `fam.base`. DO NOT EDIT.
package agentname

import (
	"encoding"
	"fmt"
)

// The coding agents that FAM manages.
type AgentName string

const (
	Claude      AgentName = "claude"
	Codex       AgentName = "codex"
	Antigravity AgentName = "antigravity"
)

// String returns the string representation of AgentName
func (rcv AgentName) String() string {
	return string(rcv)
}

var _ encoding.BinaryUnmarshaler = new(AgentName)

// UnmarshalBinary implements encoding.BinaryUnmarshaler for AgentName.
func (rcv *AgentName) UnmarshalBinary(data []byte) error {
	switch str := string(data); str {
	case "claude":
		*rcv = Claude
	case "codex":
		*rcv = Codex
	case "antigravity":
		*rcv = Antigravity
	default:
		return fmt.Errorf(`illegal: "%s" is not a valid AgentName`, str)
	}
	return nil
}
