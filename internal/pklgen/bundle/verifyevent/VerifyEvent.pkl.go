// Code generated from Pkl module `fam.Bundle`. DO NOT EDIT.
package verifyevent

import (
	"encoding"
	"fmt"
)

// The event after which a verify script runs.
//
// - `"fresh_shell"`: FAM opens new interactive and noninteractive SSH shells.
// - `"reconnect"`: FAM closes the shared SSH connection first, then opens a new one.
// - `"reboot"`: reserved. FAM version 0 does not run it.
type VerifyEvent string

const (
	FreshShell VerifyEvent = "fresh_shell"
	Reconnect  VerifyEvent = "reconnect"
	Reboot     VerifyEvent = "reboot"
)

// String returns the string representation of VerifyEvent
func (rcv VerifyEvent) String() string {
	return string(rcv)
}

var _ encoding.BinaryUnmarshaler = new(VerifyEvent)

// UnmarshalBinary implements encoding.BinaryUnmarshaler for VerifyEvent.
func (rcv *VerifyEvent) UnmarshalBinary(data []byte) error {
	switch str := string(data); str {
	case "fresh_shell":
		*rcv = FreshShell
	case "reconnect":
		*rcv = Reconnect
	case "reboot":
		*rcv = Reboot
	default:
		return fmt.Errorf(`illegal: "%s" is not a valid VerifyEvent`, str)
	}
	return nil
}
