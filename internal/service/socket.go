package service

import (
	"errors"
	"net"
	"os"
	"syscall"
	"time"
)

// A permanent lock inode prevents two cooperating processes from reclaiming the
// same socket. Do not unlink the lock file: waiters may still hold its inode.
func listenPrivate(socket string) (*net.UnixListener, func(), error) {
	if err := privateDir(socket); err != nil {
		return nil, nil, err
	}
	lockPath := socket + ".lock"
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, nil, errors.New("cannot open private socket lock")
	}
	lock := os.NewFile(uintptr(fd), "socket-lock")
	fail := func(err error) (*net.UnixListener, func(), error) { lock.Close(); return nil, nil, err }
	info, err := lock.Stat()
	if err != nil {
		return fail(errors.New("cannot inspect socket lock"))
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		return fail(errors.New("socket lock must be private, regular and owned by this user"))
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(errors.New("socket is already owned by a running service"))
	}
	pathInfo, err := os.Lstat(lockPath)
	if err != nil || !os.SameFile(info, pathInfo) {
		return fail(errors.New("socket lock changed while opening"))
	}
	if existing, err := os.Lstat(socket); err == nil {
		owner, ok := existing.Sys().(*syscall.Stat_t)
		if existing.Mode()&os.ModeSocket == 0 || !ok || owner.Uid != uint32(os.Getuid()) {
			return fail(errors.New("existing socket path is not an owned Unix socket"))
		}
		conn, dialErr := net.DialTimeout("unix", socket, 250*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return fail(errors.New("socket already has an active listener"))
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return fail(errors.New("cannot establish whether existing socket is stale"))
		}
		current, err := os.Lstat(socket)
		if err != nil || !os.SameFile(existing, current) {
			return fail(errors.New("existing socket changed during stale check"))
		}
		if err := os.Remove(socket); err != nil {
			return fail(errors.New("cannot reclaim stale socket"))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(errors.New("cannot inspect socket path"))
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return fail(errors.New("cannot listen on private socket"))
	}
	listener.SetUnlinkOnClose(false)
	bound, err := os.Lstat(socket)
	if err != nil {
		listener.Close()
		return fail(errors.New("cannot inspect bound socket"))
	}
	cleanup := func() {
		listener.Close()
		if current, err := os.Lstat(socket); err == nil && os.SameFile(bound, current) {
			os.Remove(socket)
		}
		lock.Close()
	}
	if err := os.Chmod(socket, 0600); err != nil {
		cleanup()
		return nil, nil, errors.New("cannot secure socket")
	}
	return listener, cleanup, nil
}
