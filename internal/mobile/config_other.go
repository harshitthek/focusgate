//go:build !windows

package mobile

import "os"

func replaceConfigFile(source, destination string) error {
	return os.Rename(source, destination)
}
