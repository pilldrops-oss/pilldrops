package docker

import (
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// builtinService is a built-in template that runs a prebuilt service image with
// its own entrypoint, plus any host devices it needs.
type builtinService struct {
	image   string
	warmTag string   // optional local pre-warmed image (used when present on the node)
	ports   []int    // published in addition to appPort; ports[0] is the preview target
	env     []string // container env the image's entrypoint expects
	devices []string // host devices passed through (e.g. /dev/kvm)
	// runUser is the "uid:gid" the container and every tenant-reachable
	// operation run as. Pinned here rather than inherited from the image, since
	// the warm image is baked on the node and could end as root. Empty falls
	// back to root (see rootReason).
	runUser string
	// harness injects the embedded test-session harness at boot.
	harness bool
}

// sessionPort is where the injected session harness serves; it is ports[0] of
// harness templates so the preview URL is the session page.
const sessionPort = 8765

// builtinServices are the only templates granted a host device; resolveBuiltin
// returns none when the request carries its own image. /dev/kvm is an ioctl
// surface no capability set bounds, so passing it through is a trust decision.
var builtinServices = map[string]builtinService{
	// Android 14 emulator with noVNC screen and Appium inside.
	"android": {
		image: "budtmo/docker-android:emulator_14.0",
		// First boot takes tens of minutes; the warm image is baked once per node.
		warmTag: "pilldrops/android-warm:14",
		ports:   []int{6080, 4723},
		env:     []string{"EMULATOR_DEVICE=Samsung Galaxy S10", "WEB_VNC=true", "APPIUM=true"},
		devices: []string{"/dev/kvm"},
		runUser: androidRunUser,
	},
	// Headless variant: no VNC stack, preview URL is the Appium endpoint.
	"android-headless": {
		image:   "budtmo/docker-android:emulator_14.0",
		warmTag: "pilldrops/android-warm:14",
		ports:   []int{4723},
		env:     []string{"EMULATOR_DEVICE=Samsung Galaxy S10", "APPIUM=true"},
		devices: []string{"/dev/kvm"},
		runUser: androidRunUser,
	},
	// Headless emulator plus the injected session harness; the preview URL
	// shows live progress and then the report.
	"mobile-test": {
		image:   "budtmo/docker-android:emulator_14.0",
		warmTag: "pilldrops/android-warm:14",
		ports:   []int{sessionPort, 4723},
		env:     []string{"EMULATOR_DEVICE=Samsung Galaxy S10", "APPIUM=true"},
		devices: []string{"/dev/kvm"},
		runUser: androidRunUser,
		harness: true,
	},
}

// resolveBuiltin fills spec.Template from builtinServices and returns the host
// devices to map. Requests with an explicit image are left untouched.
func resolveBuiltin(spec rt.BootSpec) (rt.BootSpec, []string) {
	if spec.Template.Image != "" {
		return spec, nil
	}
	bs, ok := builtinServices[canonicalTemplate(spec.Template.Name)]
	if !ok {
		return spec, nil
	}
	spec.Template.Image = bs.image
	spec.Template.Ports = append(append([]int{}, bs.ports...), spec.Template.Ports...)
	spec.Template.Env = append(append([]string{}, bs.env...), spec.Template.Env...)
	return spec, append([]string{}, bs.devices...)
}

// builtinRunUser is the pinned identity for a builtin template, or "" for anything else.
func builtinRunUser(templateName string) string {
	return builtinServices[canonicalTemplate(templateName)].runUser
}
