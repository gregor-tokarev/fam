// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package state

import (
	"encoding"
	"fmt"
)

// The wanted state of a bundle or a task.
//
// - `"present"`: FAM installs or writes the item.
// - `"absent"`: FAM removes the item.
type State string

const (
	Present State = "present"
	Absent  State = "absent"
)

// String returns the string representation of State
func (rcv State) String() string {
	return string(rcv)
}

var _ encoding.BinaryUnmarshaler = new(State)

// UnmarshalBinary implements encoding.BinaryUnmarshaler for State.
func (rcv *State) UnmarshalBinary(data []byte) error {
	switch str := string(data); str {
	case "present":
		*rcv = Present
	case "absent":
		*rcv = Absent
	default:
		return fmt.Errorf(`illegal: "%s" is not a valid State`, str)
	}
	return nil
}
