package domain

import "errors"

// ErrReadOnly is what every refused request wraps when k10s runs with
// --readonly, so callers can tell a refusal from a failure.
var ErrReadOnly = errors.New("read-only mode")

// ReadOnlyAllows reports whether an action may run in read-only mode. Only
// the ones that read are allowed. Every other action either changes the
// cluster (restart, scale, edit, cordon, drain, delete) or opens a session
// into it (shell, port forward), and read-only mode refuses both kinds.
func ReadOnlyAllows(id string) bool {
	switch id {
	case ADescribe, AYAML, ALogs, ATop:
		return true
	}
	return false
}
