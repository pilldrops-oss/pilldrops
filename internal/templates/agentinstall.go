package templates

import "regexp"

// InstallPackageOf extracts the package from an npm -g or pip install command.
// Returns "" for anything else, such as a curl-piped binary fetch.
func InstallPackageOf(installCmd string) string {
	if m := npmGlobalInstallRe.FindStringSubmatch(installCmd); m != nil {
		return m[1]
	}
	if m := pipInstallRe.FindStringSubmatch(installCmd); m != nil {
		return m[1]
	}
	return ""
}

var (
	npmGlobalInstallRe = regexp.MustCompile(`-g\s+(\S+)`)
	pipInstallRe       = regexp.MustCompile(`pip3?\s+install\s+(?:-\S+\s+)*(\S+)`)
)

var packageManagerRe = regexp.MustCompile(`\b(?:npm|pip3?)\b`)

// IsPackageManagerInstall reports whether installCmd is an npm or pip
// invocation, so callers can tell an expected empty InstallPackageOf from a parsing gap.
func IsPackageManagerInstall(installCmd string) bool {
	return packageManagerRe.MatchString(installCmd)
}
