//go:build !windows

package modelstudio

import "os"

func replaceVerificationCodeFile(source, destination string) error {
	return os.Rename(source, destination)
}
