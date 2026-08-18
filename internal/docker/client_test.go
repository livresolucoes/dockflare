package docker

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCgroup(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(f, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSelfContainerID_CgroupV1(t *testing.T) {
	id := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	cgroupFile = writeCgroup(t, "12:devices:/docker/"+id+"\n")
	t.Cleanup(func() { cgroupFile = "/proc/self/cgroup" })

	got, err := SelfContainerID()
	if err != nil {
		t.Fatal(err)
	}
	if got != id[:12] {
		t.Errorf("got %q, want %q", got, id[:12])
	}
}

func TestSelfContainerID_CgroupNotFound(t *testing.T) {
	cgroupFile = "/nonexistent/cgroup"
	t.Cleanup(func() { cgroupFile = "/proc/self/cgroup" })

	// Hostname fallback: if hostname is not 12-char hex, should return error.
	// We can't control hostname in tests, so just verify the function runs without panic.
	_, _ = SelfContainerID()
}

func TestSelfContainerID_CgroupNoDockerPath(t *testing.T) {
	cgroupFile = writeCgroup(t, "0::/\n1:memory:/system.slice/docker.service\n")
	t.Cleanup(func() { cgroupFile = "/proc/self/cgroup" })

	// Should fall through to hostname; result depends on environment.
	_, _ = SelfContainerID()
}

func TestIsHex(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"a1b2c3d4e5f6", true},
		{"000000000000", true},
		{"ffffffffffff", true},
		{"A1B2C3D4E5F6", false}, // uppercase not valid
		{"a1b2c3d4e5g6", false}, // 'g' invalid
		{"", true},              // vacuously true
	}
	for _, tc := range cases {
		if got := isHex(tc.s); got != tc.want {
			t.Errorf("isHex(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}
