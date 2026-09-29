package nodeboot

import "testing"

// A named snapshot's bytes are the image `docker commit` wrote, named after
// the sandbox it was captured from, while the build is indexed under the
// snapshot's template id. The fetcher used to refuse that mismatch outright,
// so a container node could capture a snapshot and never boot one. It now
// names the loaded image after the tag the index resolved — which needs the
// loaded reference read back out of docker's own output.
func TestLoadedImageRef(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, out, want string }{
		{"tagged", "Loaded image: pd-ckpt-sb1-export-b7:latest\n", "pd-ckpt-sb1-export-b7:latest"},
		{"untagged", "Loaded image ID: sha256:abc123\n", "sha256:abc123"},
		{"several take the last", "Loaded image: a:1\nLoaded image: b:2\n", "b:2"},
		{"noise around it", "Some layer chatter\nLoaded image: a:1\ndone\n", "a:1"},
		// Nothing recognised must stay empty: the caller reports what docker
		// said rather than tagging something it did not identify.
		{"nothing recognised", "no such file or directory\n", ""},
		{"prefix with no value", "Loaded image:\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := loadedImageRef(tc.out); got != tc.want {
				t.Errorf("loadedImageRef(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}
