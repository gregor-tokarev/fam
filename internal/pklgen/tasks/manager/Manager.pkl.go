// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package manager

import (
	"encoding"
	"fmt"
)

// A package manager for [Package.package].
//
// - `"npm"`, `"bun"`, `"vite"` (Vite+ `vp install -g`): global JavaScript packages, as the user.
// - `"uv"`, `"cargo"`, `"go"`: user tools, without sudo.
// - `"system"`: brew on macOS, or the package manager of the Linux distribution.
// - `"brew"`: Homebrew, as the user.
// - `"apt"`, `"dnf"`, `"pacman"`, `"apk"`: the OS package manager. FAM runs it with `sudo`.
// - `"yay"`: AUR packages on Arch Linux, as the user (yay runs `sudo` itself).
type Manager string

const (
	Uv     Manager = "uv"
	Cargo  Manager = "cargo"
	Go     Manager = "go"
	System Manager = "system"
	Npm    Manager = "npm"
	Bun    Manager = "bun"
	Vite   Manager = "vite"
	Brew   Manager = "brew"
	Apt    Manager = "apt"
	Dnf    Manager = "dnf"
	Pacman Manager = "pacman"
	Yay    Manager = "yay"
	Apk    Manager = "apk"
)

// String returns the string representation of Manager
func (rcv Manager) String() string {
	return string(rcv)
}

var _ encoding.BinaryUnmarshaler = new(Manager)

// UnmarshalBinary implements encoding.BinaryUnmarshaler for Manager.
func (rcv *Manager) UnmarshalBinary(data []byte) error {
	switch str := string(data); str {
	case "uv":
		*rcv = Uv
	case "cargo":
		*rcv = Cargo
	case "go":
		*rcv = Go
	case "system":
		*rcv = System
	case "npm":
		*rcv = Npm
	case "bun":
		*rcv = Bun
	case "vite":
		*rcv = Vite
	case "brew":
		*rcv = Brew
	case "apt":
		*rcv = Apt
	case "dnf":
		*rcv = Dnf
	case "pacman":
		*rcv = Pacman
	case "yay":
		*rcv = Yay
	case "apk":
		*rcv = Apk
	default:
		return fmt.Errorf(`illegal: "%s" is not a valid Manager`, str)
	}
	return nil
}
