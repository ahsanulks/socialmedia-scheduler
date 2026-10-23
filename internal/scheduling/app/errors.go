package app

import "fmt"

// UnsupportedPlatformError reports a platform that has no registered adapter.
type UnsupportedPlatformError struct {
	Platform string
}

func (e UnsupportedPlatformError) Error() string {
	return fmt.Sprintf("unsupported platform: %q", e.Platform)
}

// PlatformLimitError reports a publication the target platform cannot express.
type PlatformLimitError struct {
	Platform string
	Field    string
	Message  string
}

func (e PlatformLimitError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}
