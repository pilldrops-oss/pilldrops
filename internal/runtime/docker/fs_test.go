package docker

import "testing"

func TestDockerFSAbs(t *testing.T) {
	d := &dockerFS{cid: "c"}
	cases := []struct{ in, want string }{
		{"/tmp/shots/01.png", "/tmp/shots/01.png"},
		{"/home/androidusr/summary.txt", "/home/androidusr/summary.txt"},
		{"/test/artifacts/video.mp4", "/test/artifacts/video.mp4"},
		{"/tmp/../tmp/x", "/tmp/x"},
		{"app.py", "/workspace/app.py"},
		{"sub/dir/f", "/workspace/sub/dir/f"},
		{"../../etc/passwd", "/workspace/etc/passwd"},
		{"./x", "/workspace/x"},
	}
	for _, c := range cases {
		if got := d.abs(c.in); got != c.want {
			t.Errorf("abs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
