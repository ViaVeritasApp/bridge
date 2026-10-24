package config

// RunAs is who a terminal or a command from ViaVeritas runs as on this computer.
type RunAs string

const (
	// RunAsUser runs it as the person signed in at the computer: refused while nobody is.
	RunAsUser RunAs = "user"
	// RunAsSystem runs it as the computer's administrator: root, or SYSTEM on Windows.
	RunAsSystem RunAs = "system"
)

// Known reports whether r is a choice the bridge knows.
func (r RunAs) Known() bool { return r == RunAsUser || r == RunAsSystem }

// SessionsRunAs is who terminals and commands run as, the signed-in person until someone chooses.
func (c Config) SessionsRunAs() RunAs {
	if c.RunAs == RunAsSystem {
		return RunAsSystem
	}
	return RunAsUser
}

// SessionNotices reports whether this computer's screen says when ViaVeritas
// opens a terminal or runs a command: on until someone here switches it off.
func (c Config) SessionNotices() bool { return !c.HideSessionNotices }
