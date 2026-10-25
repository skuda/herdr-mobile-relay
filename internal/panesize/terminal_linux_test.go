package panesize

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func procStatFixture(pid int, comm string, tty int32, start uint64) []byte {
	fields := []string{"S", "1", "2", "3", strconv.FormatInt(int64(tty), 10)}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, strconv.FormatUint(start, 10), "0", "0")
	return []byte(fmt.Sprintf("%d (%s) %s\n", pid, comm, strings.Join(fields, " ")))
}
func encodedProcTTY(major, minor uint32) int32 {
	return int32((major << 8) | (minor & 0xff) | ((minor & 0xfff00) << 12))
}
func TestProcTTYStatParserAndDeviceEncoding(t *testing.T) {
	for _, comm := range []string{"shell", "shell with spaces", `weird ) (name) \\ name`} {
		got, err := parseProcTTYStat(procStatFixture(123, comm, encodedProcTTY(136, 7), 456), 123)
		if err != nil || got != (procTTYStat{pid: 123, tty: uint32(encodedProcTTY(136, 7)), start: 456}) {
			t.Errorf("comm=%q parsed=%+v error=%v", comm, got, err)
		}
	}
	for _, minor := range []uint32{0, 7, 255, 256, 4095, 524288, 1048575} {
		tty := encodedProcTTY(136, minor)
		path, rdev, err := procTTYPath(uint32(tty))
		if err != nil || path != "/dev/pts/"+strconv.FormatUint(uint64(minor), 10) || rdev != unix.Mkdev(136, minor) {
			t.Errorf("minor=%d signed=%d path=%q rdev=%d error=%v", minor, tty, path, rdev, err)
		}
		if _, err := parseProcTTYStat(procStatFixture(123, "signed tty", tty, 456), 123); err != nil {
			t.Errorf("signed tty=%d rejected: %v", tty, err)
		}
	}
	for _, major := range []uint32{0, 4, 5, 137, 4095} {
		if _, _, err := procTTYPath(uint32(encodedProcTTY(major, 1))); err == nil {
			t.Errorf("unsupported major=%d accepted", major)
		}
	}
}
func TestProcTTYMalformedStatRefuses(t *testing.T) {
	valid := string(procStatFixture(123, "shell", encodedProcTTY(136, 7), 456))
	for name, data := range map[string]string{
		"absent": "", "short": "123 (shell) S 1 2 3", "no-open": "123 shell) S 1", "no-close": "123 (shell S 1", "wrong-pid": strings.Replace(valid, "123 (", "124 (", 1),
		"missing-field-separator": strings.Replace(valid, ") S", ")S", 1),
		"zero-tty":                string(procStatFixture(123, "shell", 0, 456)), "zero-start": string(procStatFixture(123, "shell", encodedProcTTY(136, 7), 0)),
		"bad-tty": strings.Replace(valid, "34823", "invalid", 1), "overflow-tty": strings.Replace(valid, "34823", "4294967296", 1), "bad-start": strings.Replace(valid, "456", "-1", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseProcTTYStat([]byte(data), 123); err == nil {
				t.Errorf("malformed stat accepted: %q", data)
			}
		})
	}
	if _, err := parseProcTTYStat([]byte(valid), 0); err == nil {
		t.Error("invalid pid accepted")
	}
}
func TestProcTTYResolverUsesControllingTTYAndChecksBirth(t *testing.T) {
	for _, failure := range []string{"none", "stat-missing", "permission", "short", "pid-reused", "tty-changed", "process-exited", "missing-device", "not-character", "namespace-mismatch", "cancelled", "cancel-after-device"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads, stats := 0, 0
			resolver := procTTYResolver{
				readFile: func(path string) ([]byte, error) {
					reads++
					if path != "/proc/123/stat" {
						t.Errorf("resolver read non-stat path %q", path)
					}
					if failure == "stat-missing" || failure == "process-exited" && reads == 2 {
						return nil, os.ErrNotExist
					}
					if failure == "permission" {
						return nil, os.ErrPermission
					}
					if failure == "short" {
						return []byte("123 (x) S"), nil
					}
					start, tty := uint64(456), encodedProcTTY(136, 7)
					if reads == 2 && failure == "pid-reused" {
						start++
					}
					if reads == 2 && failure == "tty-changed" {
						tty = encodedProcTTY(136, 8)
					}
					return procStatFixture(123, "shell with ) spaces", tty, start), nil
				},
				device: func(path string) (uint64, bool, error) {
					stats++
					if path != "/dev/pts/7" {
						t.Errorf("candidate=%q", path)
					}
					if failure == "missing-device" {
						return 0, false, os.ErrNotExist
					}
					if failure == "not-character" {
						return unix.Mkdev(136, 7), false, nil
					}
					if failure == "namespace-mismatch" {
						return unix.Mkdev(136, 8), true, nil
					}
					if failure == "cancel-after-device" {
						cancel()
					}
					return unix.Mkdev(136, 7), true, nil
				},
			}
			if failure == "cancelled" {
				cancel()
			}
			path, start, rdev, err := resolver.ttyIdentity(ctx, 123)
			if failure == "none" {
				if err != nil || path != "/dev/pts/7" || start != 456 || rdev != unix.Mkdev(136, 7) || reads != 2 || stats != 1 {
					t.Errorf("resolution=%q/%d/%d error=%v reads=%d stats=%d", path, start, rdev, err, reads, stats)
				}
			} else if err == nil {
				t.Error("uncertain controlling terminal accepted")
			}
			if failure == "cancelled" && (reads != 0 || stats != 0) {
				t.Error("cancelled resolution did I/O")
			}
		})
	}
}
func TestProcTTYResolverInvalidPIDAndUnsupportedTTYDoNoDeviceLookup(t *testing.T) {
	for _, pid := range []int{0, -1} {
		resolver := procTTYResolver{
			readFile: func(string) ([]byte, error) { t.Error("invalid PID read procfs"); return nil, os.ErrNotExist },
			device:   func(string) (uint64, bool, error) { t.Error("invalid PID inspected device"); return 0, false, nil },
		}
		if _, err := resolver.TTYPath(context.Background(), pid); err == nil {
			t.Error("invalid PID resolved")
		}
	}
	for _, tty := range []int32{0, encodedProcTTY(4, 1), encodedProcTTY(137, 1)} {
		resolver := procTTYResolver{
			readFile: func(string) ([]byte, error) { return procStatFixture(123, "unsupported tty", tty, 456), nil },
			device: func(string) (uint64, bool, error) {
				t.Error("unsupported tty inspected a candidate device")
				return 0, false, nil
			},
		}
		if _, err := resolver.TTYPath(context.Background(), 123); err == nil {
			t.Errorf("unsupported tty=%d resolved", tty)
		}
	}
}

func TestProcTTYRedirectedStdinDoesNotChangeResolution(t *testing.T) {
	resolver := procTTYResolver{
		readFile: func(path string) ([]byte, error) {
			if strings.Contains(path, "/fd/") {
				t.Fatal("resolver inferred controlling terminal from stdin")
			}
			return procStatFixture(123, "stdin is a pipe", encodedProcTTY(136, 9), 1), nil
		},
		device: func(path string) (uint64, bool, error) { return unix.Mkdev(136, 9), true, nil },
	}
	if path, err := resolver.TTYPath(context.Background(), 123); err != nil || path != "/dev/pts/9" {
		t.Errorf("redirected-stdin path=%q error=%v", path, err)
	}
}
func TestProcTTYSelfStatSmoke(t *testing.T) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Skipf("procfs unavailable: %v", err)
	}
	text := string(data)
	close := strings.LastIndexByte(text, ')')
	if close >= 0 {
		fields := strings.Fields(text[close+1:])
		if len(fields) > 4 && fields[4] == "0" {
			t.Skip("self process has tty_nr 0")
		}
	}
	parsed, err := parseProcTTYStat(data, os.Getpid())
	if err != nil {
		t.Fatalf("self stat parser failed: %v", err)
	}
	path, _, err := procTTYPath(parsed.tty)
	if err != nil {
		t.Skip("self controlling terminal is not devpts")
	}
	got, err := platformTTYResolver().TTYPath(context.Background(), os.Getpid())
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if got != path {
		t.Errorf("self tty=%q want=%q", got, path)
	}
}
