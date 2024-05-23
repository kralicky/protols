package version

import "fmt"

var (
	Version   = "v0.0.0-dev"
	GitCommit = "HEAD"
)

// FriendlyVersion returns the version to be displayed on running --version
func FriendlyVersion() string {
	return fmt.Sprintf("%s (%s)", Version, GitCommit)
}
