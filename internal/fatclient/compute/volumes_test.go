package compute

import (
	"strings"
	"testing"
)

// TestBuildDockerArgsVolumes — author-declared volumes are
// emitted as `-v host:container[:options]` VERBATIM. Crucially
// NO `:z` is auto-appended (that footgun-relabels arbitrary
// external reference DBs on SELinux hosts — see ISSUE-4 review);
// an author who wants a relabel writes it in the options
// segment and it passes through untouched. Every volume bind
// must precede the image so docker parses them as run flags.
func TestBuildDockerArgsVolumes(t *testing.T) {
	t.Setenv("ENJU_SHARED_ROOT", "")

	spec := Spec{
		Container:  "staphb/nanoplot:latest",
		ScriptPath: "/host/ws/run.sh",
		Volumes: []string{
			"/data/refs",            // bare → /data/refs:/data/refs
			"/data/raw:/inputs",     // host:container, no relabel
			"/data/db:/db:ro",       // ro, still no auto-z
			"/data/shared:/sh:ro,z", // author opts INTO relabel → verbatim
		},
	}
	args, err := BuildContainerArgs(RuntimeDocker, spec, nil, "/host/ws", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}

	wantBinds := []string{
		"/data/refs:/data/refs",
		"/data/raw:/inputs",
		"/data/db:/db:ro",
		"/data/shared:/sh:ro,z",
	}
	for _, want := range wantBinds {
		if !hasFlagValue(args, "-v", want) {
			t.Errorf("missing volume bind: -v %s\nargs: %v", want, args)
		}
	}

	// No declared bind may carry an Enju-injected relabel. Only
	// the explicit author opt-in (/data/shared) ends in ",z".
	for _, a := range args {
		if a == "/data/refs:/data/refs:z" ||
			a == "/data/raw:/inputs:z" ||
			a == "/data/db:/db:ro,z" {
			t.Errorf("Enju auto-appended an SELinux relabel to a declared volume: %q", a)
		}
	}

	imgIdx := argIndex(args, spec.Container)
	if imgIdx < 0 {
		t.Fatalf("image %q not in args: %v", spec.Container, args)
	}
	for _, bind := range wantBinds {
		if bi := argIndex(args, bind); bi < 0 || bi > imgIdx {
			t.Errorf("volume bind %q must precede the image (bind@%d, image@%d)", bind, bi, imgIdx)
		}
	}
}

// TestBuildApptainerArgsVolumes — the apptainer mirror:
// `--bind host:container[:options]`, options verbatim, no
// Enju-injected SELinux relabel (this side never had one).
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

// TestParseVolumeSpec pins the host/container/options split
// (options is opaque — ro/rw/z/Z/combos pass through untouched)
// and the defensive error cases the wrapper guards (an async
// spec from an older binary, or a direct Spec construction,
// never passes through the YAML validator).
func TestParseVolumeSpec(t *testing.T) {
	ok := []struct {
		in                       string
		host, container, options string
	}{
		{"/data/refs", "/data/refs", "/data/refs", ""},
		{"/data/raw:/inputs", "/data/raw", "/inputs", ""},
		{"/data/db:/db:ro", "/data/db", "/db", "ro"},
		{"/data/db:/db:rw", "/data/db", "/db", "rw"},
		{"/data/db:/db:ro,z", "/data/db", "/db", "ro,z"}, // opaque combo
		{"/data/db:/db:Z", "/data/db", "/db", "Z"},       // opaque, not allowlisted
		{"/host:", "/host", "/host", ""},                 // empty container falls back to host
	}
	for _, c := range ok {
		h, ctr, o, err := parseVolumeSpec(c.in)
		if err != nil {
			t.Errorf("parseVolumeSpec(%q) unexpected error: %v", c.in, err)
			continue
		}
		if h != c.host || ctr != c.container || o != c.options {
			t.Errorf("parseVolumeSpec(%q) = (%q,%q,%q), want (%q,%q,%q)",
				c.in, h, ctr, o, c.host, c.container, c.options)
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
