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

// MediaRequiredError reports a post without media for a platform that cannot
// post text alone (e.g. Instagram).
type MediaRequiredError struct {
	Platform string
}

func (e MediaRequiredError) Error() string {
	return fmt.Sprintf("platform %q requires media", e.Platform)
}

// UnsupportedMediaError reports a media kind the platform's adapter does not
// accept, including any media sent to a text-only platform.
type UnsupportedMediaError struct {
	Platform string
	Kind     string
}

func (e UnsupportedMediaError) Error() string {
	return fmt.Sprintf("platform %q does not accept %q media", e.Platform, e.Kind)
}
