//go:build linux

package webdav

import "golang.org/x/sys/unix"

func setAttr(file, name, value string) { _ = unix.Setxattr(file, name, []byte(value), 0) }

func getAttr(file, name string) (string, bool) {
	buf := make([]byte, 256)
	n, err := unix.Getxattr(file, name, buf)
	if err != nil || n <= 0 || n > len(buf) {
		return "", false
	}
	return string(buf[:n]), true
}
