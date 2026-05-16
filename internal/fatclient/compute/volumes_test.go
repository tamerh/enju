package compute

import (
	"strings"
	"testing"
)

// TestBuildDockerArgsVolumes — author-declared volumes become
// `-v host:container[:mode],z` (the :z relabel mirrors the
// workspace/snapshot binds; an explicit mode collapses to
// "mode,z" like the snapshot's ":ro,z"). All three accepted
// forms are exercised, and every volume bind must precede the
// image so docker parses them as run flags.
func TestBuildDockerArgsVolumes(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")

	spec := Spec{
		Container:  "staphb/nanoplot:latest",
		ScriptPath: "/host/ws/run.sh",
		Volumes: []string{
			"/data/refs",              // bare → /data/refs:/data/refs:z
			"/data/raw:/inputs",       // host:container → :z
			"/data/db:/db:ro",         // host:container:mode → :ro,z
		},
	}
	args, err := BuildContainerArgs(RuntimeDocker, spec, nil, "/host/ws", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"/data/refs:/data/refs:z",
		"/data/raw:/inputs:z",
		"/data/db:/db:ro,z",
	} {
		if !hasFlagValue(args, "-v", want) {
			t.Errorf("missing volume bind: -v %s\nargs: %v", want, args)
		}
	}

	imgIdx := argIndex(args, spec.Container)
	if imgIdx < 0 {
		t.Fatalf("image %q not in args: %v", spec.Container, args)
	}
	for _, bind := range []string{
		"/data/refs:/data/refs:z",
		"/data/raw:/inputs:z",
		"/data/db:/db:ro,z",
	} {
		if bi := argIndex(args, bind); bi < 0 || bi > imgIdx {
			t.Errorf("volume bind %q must precede the image (bind@%d, image@%d)", bind, bi, imgIdx)
		}
	}
}

// TestBuildApptainerArgsVolumes — the apptainer mirror:
// `--bind host:container[:mode]`, no SELinux relabel (apptainer's
// user-namespace mode doesn't use it, same as its workspace bind).
func TestBuildApptainerArgsVolumes(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")

	spec := Spec{
		Container:  "docker://alpine:3.19",
		ScriptPath: "/host/ws/run.sh",
		Volumes: []string{
			"/data/refs",
			"/data/raw:/inputs",
			"/data/db:/db:ro",
		},
	}
	args, err := BuildContainerArgs(RuntimeApptainer, spec, nil, "/host/ws", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/data/refs:/data/refs",
		"/data/raw:/inputs",
		"/data/db:/db:ro",
	} {
		if !hasFlagValue(args, "--bind", want) {
			t.Errorf("missing volume bind: --bind %s\nargs: %v", want, args)
		}
	}
	// No docker-style :z relabel should leak into apptainer binds.
	for _, a := range args {
		if strings.HasPrefix(a, "/data/") && strings.HasSuffix(a, ",z") {
			t.Errorf("apptainer bind carries a docker :z relabel: %q", a)
		}
	}
}

// TestBuildContainerArgsNoVolumesNoExtraBinds — an empty
// Volumes list adds nothing beyond the implicit workspace bind.
// Guards against a spurious "-v :" or similar from the loop.
func TestBuildContainerArgsNoVolumesNoExtraBinds(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")

	spec := Spec{Container: "alpine", ScriptPath: "/host/ws/run.sh"}
	args, err := BuildContainerArgs(RuntimeDocker, spec, nil, "/host/ws", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	vCount := 0
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-v" {
			vCount++
		}
	}
	// Only the workspace bind (shared root unset, no scratch/snapshot).
	if vCount != 1 {
		t.Errorf("expected exactly 1 -v (workspace), got %d\nargs: %v", vCount, args)
	}
}

// TestParseVolumeSpec pins the host/container/mode split and
// the defensive error cases the wrapper guards (an async spec
// from an older binary, or a direct Spec construction, never
// passes through the YAML validator).
func TestParseVolumeSpec(t *testing.T) {
	ok := []struct {
		in                      string
		host, container, mode string
	}{
		{"/data/refs", "/data/refs", "/data/refs", ""},
		{"/data/raw:/inputs", "/data/raw", "/inputs", ""},
		{"/data/db:/db:ro", "/data/db", "/db", "ro"},
		{"/data/db:/db:rw", "/data/db", "/db", "rw"},
		{"/host:", "/host", "/host", ""}, // empty container falls back to host
	}
	for _, c := range ok {
		h, ctr, m, err := parseVolumeSpec(c.in)
		if err != nil {
			t.Errorf("parseVolumeSpec(%q) unexpected error: %v", c.in, err)
			continue
		}
		if h != c.host || ctr != c.container || m != c.mode {
			t.Errorf("parseVolumeSpec(%q) = (%q,%q,%q), want (%q,%q,%q)",
				c.in, h, ctr, m, c.host, c.container, c.mode)
		}
	}

	bad := []string{
		"",                // empty entry
		":/container",     // empty host
		"/a:/b:ro:extra",  // too many segments
	}
	for _, in := range bad {
		if _, _, _, err := parseVolumeSpec(in); err == nil {
			t.Errorf("parseVolumeSpec(%q): expected error, got nil", in)
		}
	}
}
