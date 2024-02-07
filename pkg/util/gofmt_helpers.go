// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
package util

import (
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
)

//
// =================================================================
// This file contains some vendored code from src/cmd/gofmt/gofmt.go
// =================================================================
//

// fdSem guards the number of concurrently-open file descriptors.
//
// For now, this is arbitrarily set to 200, based on the observation that many
// platforms default to a kernel limit of 256. Ideally, perhaps we should derive
// it from rlimit on platforms that support that system call.
//
// File descriptors opened from outside of this package are not tracked,
// so this limit may be approximate.
var fdSem = make(chan bool, 200)

// OverwriteFile updates a file with the new formatted data.
func OverwriteFile(filename string, orig, formatted []byte, perm fs.FileMode, size int64) error {
	// Make a temporary backup file before rewriting the original file.
	bakname, err := backupFile(filename, orig, perm)
	if err != nil {
		return err
	}

	fdSem <- true
	defer func() { <-fdSem }()

	fout, err := os.OpenFile(filename, os.O_WRONLY, perm)
	if err != nil {
		// We couldn't even open the file, so it should
		// not have changed.
		os.Remove(bakname)
		return err
	}
	defer fout.Close() // for error paths

	restoreFail := func(err error) {
		fmt.Fprintf(os.Stderr, "gofmt: %s: error restoring file to original: %v; backup in %s\n", filename, err, bakname)
	}

	n, err := fout.Write(formatted)
	if err == nil && int64(n) < size {
		err = fout.Truncate(int64(n))
	}

	if err != nil {
		// Rewriting the file failed.

		if n == 0 {
			// Original file unchanged.
			os.Remove(bakname)
			return err
		}

		// Try to restore the original contents.

		no, erro := fout.WriteAt(orig, 0)
		if erro != nil {
			// That failed too.
			restoreFail(erro)
			return err
		}

		if no < n {
			// Original file is shorter. Truncate.
			if erro = fout.Truncate(int64(no)); erro != nil {
				restoreFail(erro)
				return err
			}
		}

		if erro := fout.Close(); erro != nil {
			restoreFail(erro)
			return err
		}

		// Original contents restored.
		os.Remove(bakname)
		return err
	}

	if err := fout.Close(); err != nil {
		restoreFail(err)
		return err
	}

	// File updated.
	os.Remove(bakname)
	return nil
}

// backupFile writes data to a new file named filename<number> with permissions perm,
// with <number> randomly chosen such that the file name is unique. backupFile returns
// the chosen file name.
func backupFile(filename string, data []byte, perm fs.FileMode) (string, error) {
	fdSem <- true
	defer func() { <-fdSem }()

	nextRandom := func() string {
		return strconv.Itoa(rand.Int())
	}

	dir, base := filepath.Split(filename)
	var (
		bakname string
		f       *os.File
	)
	for {
		bakname = filepath.Join(dir, base+"."+nextRandom())
		var err error
		f, err = os.OpenFile(bakname, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if err == nil {
			break
		}
		if err != nil && !os.IsExist(err) {
			return "", err
		}
	}

	// write data to backup file
	_, err := f.Write(data)
	if err1 := f.Close(); err == nil {
		err = err1
	}

	return bakname, err
}
