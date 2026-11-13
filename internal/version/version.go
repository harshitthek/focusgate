package version

import (
	"fmt"
	"runtime"
)

// Build metadata set via -ldflags during compilation.
var (
	Version = "1.0.0"
	Commit  = "dev"
	Date    = "unknown"
)

// Info holds system and build metadata.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Get returns the current runtime and build information.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}

// String formats the version info for CLI output.
func (i Info) String() string {
	return fmt.Sprintf("FocusGate v%s (%s/%s, commit %s, built %s)", i.Version, i.OS, i.Arch, i.Commit, i.Date)
}
