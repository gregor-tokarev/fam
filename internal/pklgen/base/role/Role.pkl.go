// Code generated from Pkl module `fam.base`. DO NOT EDIT.
package role

import (
	"encoding"
	"fmt"
)

// The role of a machine.
//
// - `"cockpit"`: the control machine. FAM runs here and runs its commands locally.
// - `"worker"`: a machine that FAM controls over SSH.
type Role string

const (
	Cockpit Role = "cockpit"
	Worker  Role = "worker"
)

// String returns the string representation of Role
func (rcv Role) String() string {
	return string(rcv)
}

var _ encoding.BinaryUnmarshaler = new(Role)

// UnmarshalBinary implements encoding.BinaryUnmarshaler for Role.
func (rcv *Role) UnmarshalBinary(data []byte) error {
	switch str := string(data); str {
	case "cockpit":
		*rcv = Cockpit
	case "worker":
		*rcv = Worker
	default:
		return fmt.Errorf(`illegal: "%s" is not a valid Role`, str)
	}
	return nil
}
