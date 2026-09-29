package cloudinit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute the generated disk helper with inert devices and command doubles.
// A formatter writes a sentinel so these assertions check effects, not strings.
func TestDiskRecoveryOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		fresh, format, warning bool
	}{
		{"healthy", false, false, false}, {"unmounted", false, false, false},
		{"new", true, true, false}, {"unrecognized", false, true, true},
		{"corrupt", false, true, true}, {"xfs-corrupt", false, true, true},
		{"corrupt-abort", false, true, true}, {"check-io-error", false, false, true},
		{"probe-error", false, false, true}, {"missing", false, false, true},
		{"readonly", false, false, true}, {"busy", false, false, true},
		{"mount-permission", false, false, true}, {"io-error", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			devdir := filepath.Join(dir, "dev")
			for _, d := range []string{bin, devdir, filepath.Join(dir, "data")} {
				if err := os.Mkdir(d, 0700); err != nil {
					t.Fatal(err)
				}
			}
			serial := "abcde234567abcde2345"
			device := filepath.Join(devdir, "virtio-"+serial)
			if tc.name != "missing" {
				if err := os.WriteFile(device, []byte("existing-data"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "fstab"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			stub := `#!/bin/bash
set -eu
case "$(basename "$0")" in
 sleep) exit 0 ;;
 timeout) shift 2; exec "$@" ;;
 blkid)
  test "$CASE" != probe-error || exit 4
  if [[ "$CASE" == new || "$CASE" == unrecognized || "$CASE" == readonly || "$CASE" == io-error ]]; then test -f "$FIXTURE/formatted" || exit 2; fi
  printf 'UUID=test-uuid\nTYPE=%s\n' "${FS_TYPE:-ext4}" ;;
 lsblk)
  case "$*" in *MAJ:MIN*) echo 252:1;; *TYPE*) echo disk;; esac ;;
 mountpoint) [[ "$CASE" == healthy || "$CASE" == busy ]] || test -f "$FIXTURE/mounted" ;;
 findmnt)
  case "$*" in *MAJ:MIN*) echo 252:1;; *OPTIONS*) if [[ "$CASE" == busy ]]; then echo ro; else echo rw; fi;; esac ;;
 umount) exit 1 ;;
 blockdev) if [[ "$CASE" == readonly ]]; then echo 1; else echo 0; fi ;;
 dd) [[ "$CASE" != io-error ]] ;;
 mount)
  [[ "$CASE" != mount-permission ]] || exit 32
  if [[ "$CASE" == corrupt || "$CASE" == corrupt-abort || "$CASE" == check-io-error || "$CASE" == xfs-corrupt ]]; then test -f "$FIXTURE/formatted" || exit 32; fi
  touch "$FIXTURE/mounted" ;;
 e2fsck)
  case "$CASE" in
   corrupt) exit 4;;
   corrupt-abort) echo 'Root inode is not a directory; aborting.'; exit 12;;
   check-io-error) echo 'Input/output error while reading'; exit 12;;
  esac ;;
 xfs_repair) if [[ "$CASE" == xfs-corrupt ]]; then exit 1; else exit 0; fi ;;
 mkfs.ext4|mkfs.xfs) printf fresh > "$DEVICE"; touch "$FIXTURE/formatted" ;;
 barn-warning) printf '%s %s\n' "$1" "$2" >> "$FIXTURE/warnings" ;;
 *) exit 97 ;;
esac
`
			for _, name := range []string{"sleep", "timeout", "blkid", "lsblk", "mountpoint", "findmnt", "umount", "blockdev", "dd", "mount", "e2fsck", "xfs_repair", "mkfs.ext4", "mkfs.xfs", "barn-warning"} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0700); err != nil {
					t.Fatal(err)
				}
			}
			fs := "ext4"
			if tc.name == "xfs-corrupt" {
				fs = "xfs"
			}
			script := renderDiskScript([]Disk{{Serial: serial, Mount: filepath.Join(dir, "data"), Filesystem: fs, Fresh: tc.fresh}})
			script = strings.NewReplacer("/dev/disk/by-id", devdir, "/var/lib/barn", dir, "/etc/fstab", filepath.Join(dir, "fstab"), "/usr/local/libexec/barn-warning", filepath.Join(bin, "barn-warning"), `[[ -b "${dev}" ]]`, `[[ -f "${dev}" ]]`).Replace(script)
			cmd := exec.Command("bash")
			cmd.Stdin = strings.NewReader(script)
			cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "CASE="+tc.name, "FIXTURE="+dir, "DEVICE="+device, "FS_TYPE="+fs)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("helper killed the lab: %v %s", err, out)
			}
			_, err = os.Stat(filepath.Join(dir, "formatted"))
			if (err == nil) != tc.format {
				t.Fatalf("format=%v expected=%v: %s", err == nil, tc.format, out)
			}
			warnings, _ := os.ReadFile(filepath.Join(dir, "warnings"))
			if (len(warnings) > 0) != tc.warning {
				t.Fatalf("warnings=%s output=%s", warnings, out)
			}
			if tc.format && !tc.fresh && !strings.Contains(string(warnings), "previous data discarded") {
				t.Fatalf("reset was not disclosed: %s", warnings)
			}
			if !tc.format && tc.name != "missing" {
				data, _ := os.ReadFile(device)
				if string(data) != "existing-data" {
					t.Fatalf("changed usable/temporarily unavailable disk: %s", data)
				}
			}
		})
	}
}
