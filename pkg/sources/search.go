package sources

import (
	"io/fs"
	"path/filepath"
	"strings"
)

func SearchDirs(dirs ...string) []string {
	var files []string
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			a, err := filepath.Abs(dir)
			if err == nil {
				dir = a
			}
		}
		filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			name := d.Name()
			if d.IsDir() {
				if name == "node_modules" {
					return fs.SkipDir
				} else if name == "vendor" {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(name, ".proto") {
				files = append(files, path)
			}
			return nil
		})
	}
	return files
}
