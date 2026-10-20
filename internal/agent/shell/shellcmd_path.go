package shell

import (
	"path"
	"strings"
)

func posixSibling(path, newName string) string {
	if i := strings.LastIndexAny(path, "/"); i >= 0 {
		return path[:i+1] + newName
	}
	return newName
}

// remoteParent / remoteJoin build paths on the CONTROLLED end using its
// separators. The C2 may run a different OS than the shell bot, so filepath
// (which uses the C2's separators) must not be used on remote paths.
func remoteParent(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[:i]
	}
	return ""
}

func remoteJoin(dir, name string, t target) string {
	if dir == "" {
		return name
	}
	sep := "/"
	if t.isWindows() {
		sep = `\`
	}
	if strings.HasSuffix(dir, "/") || strings.HasSuffix(dir, `\`) {
		return dir + name
	}
	return dir + sep + name
}

func remotePathInside(parent, child string, t target) bool {
	p := remotePathKey(parent, t)
	c := remotePathKey(child, t)
	if p == "." {
		if c == "." {
			return true
		}
		return c != ".." && !strings.HasPrefix(c, "../") && !strings.HasPrefix(c, "/") &&
			!(len(c) >= 2 && c[1] == ':')
	}
	if p == "/" {
		return strings.HasPrefix(c, "/")
	}
	return c == p || strings.HasPrefix(c, p+"/")
}

func remotePathKey(p string, t target) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = path.Clean(p)
	if t.isWindows() {
		p = strings.ToLower(p)
	}
	return p
}
