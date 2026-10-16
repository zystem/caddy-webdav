//go:build !linux

package webdav

// no extended attributes here: the hash is taken again at every request
func setAttr(file, name, value string) {}

func getAttr(file, name string) (string, bool) { return "", false }
