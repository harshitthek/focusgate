/*
	Command android-bind packages the Go inbox for Android with SQLite's file

operations mapped to the Android-supported *at syscalls on x86-64.
*/
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/zip"
)

func main() {
	output := flag.String("o", "android/app/libs/phone.aar", "output AAR")
	flag.Parse()
	if err := bind(*output); err != nil {
		log.Fatal(err)
	}
}

func bind(output string) error {
	const libc = "modernc.org/libc"
	// Go verifies the archive against go.sum. Extract it into private build
	// storage so the daemon and the module cache keep the original dependency.
	raw, err := exec.Command("go", "mod", "download", "-json", libc).Output()
	if err != nil {
		return fmt.Errorf("resolve SQLite libc: %w", err)
	}
	var m struct{ Version, Zip string }
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "focusgate-android-bind-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	patchedModule := filepath.Join(dir, "libc")
	if err := zip.Unzip(patchedModule, module.Version{Path: libc, Version: m.Version}, m.Zip); err != nil {
		return fmt.Errorf("extract SQLite libc: %w", err)
	}
	if err := patchAndroidLibc(filepath.Join(patchedModule, "ccgo_linux_amd64.go")); err != nil {
		return err
	}
	project := filepath.Join(dir, "focusgate")
	for _, path := range []string{"androidbridge", "internal"} {
		if err := os.CopyFS(filepath.Join(project, path), os.DirFS(path)); err != nil {
			return err
		}
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(project, name), data, 0600); err != nil {
			return err
		}
	}
	edit := exec.Command("go", "mod", "edit", "-replace="+libc+"="+filepath.ToSlash(patchedModule))
	edit.Dir = project
	if output, err := edit.CombinedOutput(); err != nil {
		return fmt.Errorf("configure the Android SQLite dependency: %w: %s", err, output)
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		return err
	}
	cmd := exec.Command("gomobile", "bind", "-target=android/arm64,android/amd64", "-androidapi", "24",
		"-trimpath", "-ldflags=-s -w", "-o", output, "./androidbridge")
	cmd.Dir = project
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func patchAndroidLibc(source string) error {
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}

	// bionic uses the *at syscalls for these operations. Keep the existing musl
	// stat buffer conversion, errno handling and open flags. The ARM64 source
	// already uses these entry points. Caller counts are checked so a
	// libc update cannot silently remove part of the Android compatibility fix.
	replacements := [][2]string{
		{"X__syscall2(tls, int64(SYS_lstat), int64(path), int64(bp))",
			"X__syscall4(tls, int64(SYS_newfstatat), -100, int64(path), int64(bp), int64(AT_SYMLINK_NOFOLLOW))"},
		{"X__syscall2(tls, int64(SYS_stat), int64(path), int64(bp))",
			"X__syscall4(tls, int64(SYS_newfstatat), -100, int64(path), int64(bp), 0)"},
		{"X__syscall2(tls, int64(SYS_stat), int64(bp+144), int64(bp))",
			"X__syscall4(tls, int64(SYS_newfstatat), -100, int64(bp+144), int64(bp), 0)"},
		{"X__syscall2(tls, int64(SYS_fstat), int64(fd), int64(bp))",
			"X__syscall4(tls, int64(SYS_newfstatat), int64(fd), int64(path), int64(bp), int64(AT_EMPTY_PATH))"},
		{"___syscall_cp(tls, int64(SYS_open), int64(filename), int64(flags|Int32FromInt32(O_LARGEFILE)), Int64FromUint32(mode), 0, 0, 0)",
			"___syscall_cp(tls, int64(SYS_openat), -100, int64(filename), int64(flags|Int32FromInt32(O_LARGEFILE)), Int64FromUint32(mode), 0, 0)"},
		{"X__syscall2(tls, int64(SYS_access), int64(filename), int64(amode))",
			"X__syscall3(tls, int64(SYS_faccessat), -100, int64(filename), int64(amode))"},
		{"X__syscall3(tls, int64(SYS_readlink), int64(path), int64(buf), Int64FromUint64(bufsize))",
			"X__syscall4(tls, int64(SYS_readlinkat), -100, int64(path), int64(buf), Int64FromUint64(bufsize))"},
		{"X__syscall1(tls, int64(SYS_unlink), int64(path))",
			"X__syscall3(tls, int64(SYS_unlinkat), -100, int64(path), 0)"},
		{"X__syscall1(tls, int64(SYS_unlink), int64(bp))",
			"X__syscall3(tls, int64(SYS_unlinkat), -100, int64(bp), 0)"},
		{"X__syscall2(tls, int64(SYS_rename), int64(old), int64(new1))",
			"X__syscall5(tls, int64(SYS_renameat2), -100, int64(old), -100, int64(new1), 0)"},
	}
	patched := string(raw)
	for _, replacement := range replacements {
		expected := 1
		if replacement[0] == "X__syscall1(tls, int64(SYS_unlink), int64(path))" {
			expected = 2
		}
		if strings.Count(patched, replacement[0]) != expected {
			return fmt.Errorf("unexpected SQLite libc source, recheck Android compatibility: %s", replacement[0])
		}
		patched = strings.ReplaceAll(patched, replacement[0], replacement[1])
	}
	if err := os.Chmod(source, 0600); err != nil {
		return err
	}
	return os.WriteFile(source, []byte(patched), 0600)
}
