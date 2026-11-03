package panesize

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type procTTYResolver struct {
	readFile func(string) ([]byte, error)
	device   func(string) (uint64, bool, error)
}

type procTTYStat struct {
	pid   int
	tty   uint32
	start uint64
}

func platformTTYResolver() TTYResolver {
	return procTTYResolver{readFile: os.ReadFile, device: procTTYDevice}
}

func procTTYDevice(path string) (uint64, bool, error) {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return 0, false, err
	}
	return statDevice(stat)
}

func parseProcTTYStat(data []byte, pid int) (procTTYStat, error) {
	if pid <= 0 {
		return procTTYStat{}, ErrTTYUnavailable
	}
	text := string(data)
	open, close := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if open <= 0 || close <= open || close+1 >= len(text) || text[close+1] != ' ' {
		return procTTYStat{}, ErrTTYUnavailable
	}
	reported, err := strconv.Atoi(strings.TrimSpace(text[:open]))
	if err != nil || reported != pid {
		return procTTYStat{}, ErrTTYUnavailable
	}
	fields := strings.Fields(text[close+1:])
	if len(fields) < 20 || len(fields[0]) != 1 {
		return procTTYStat{}, ErrTTYUnavailable
	}
	tty, err := strconv.ParseInt(fields[4], 10, 32)
	if err != nil || tty == 0 {
		return procTTYStat{}, ErrTTYUnavailable
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return procTTYStat{}, ErrTTYUnavailable
	}
	return procTTYStat{pid: pid, tty: uint32(tty), start: start}, nil
}

func procTTYPath(tty uint32) (string, uint64, error) {
	major, minor := unix.Major(uint64(tty)), unix.Minor(uint64(tty))
	if major != 136 {
		return "", 0, ErrTTYUnavailable
	}
	return "/dev/pts/" + strconv.FormatUint(uint64(minor), 10), unix.Mkdev(major, minor), nil
}

func (r procTTYResolver) TTYPath(ctx context.Context, pid int) (string, error) {
	path, _, _, err := r.ttyIdentity(ctx, pid)
	return path, err
}

func (r procTTYResolver) ttyIdentity(ctx context.Context, pid int) (string, uint64, uint64, error) {
	if pid <= 0 || r.readFile == nil || r.device == nil {
		return "", 0, 0, ErrTTYUnavailable
	}
	if err := ctx.Err(); err != nil {
		return "", 0, 0, err
	}
	statPath := "/proc/" + strconv.Itoa(pid) + "/stat"
	data, err := r.readFile(statPath)
	if err != nil {
		return "", 0, 0, errors.Join(ErrTTYUnavailable, err)
	}
	before, err := parseProcTTYStat(data, pid)
	if err != nil {
		return "", 0, 0, err
	}
	path, expected, err := procTTYPath(before.tty)
	if err != nil {
		return "", 0, 0, err
	}
	actual, char, err := r.device(path)
	if err != nil || !char || actual != expected {
		return "", 0, 0, errors.Join(ErrTTYUnavailable, err)
	}
	if err := ctx.Err(); err != nil {
		return "", 0, 0, err
	}
	data, err = r.readFile(statPath)
	if err != nil {
		return "", 0, 0, errors.Join(ErrTTYUnavailable, err)
	}
	after, err := parseProcTTYStat(data, pid)
	if err != nil || before != after {
		return "", 0, 0, errors.Join(ErrTTYUnavailable, err)
	}
	if err := ctx.Err(); err != nil {
		return "", 0, 0, err
	}
	return path, before.start, expected, nil
}
