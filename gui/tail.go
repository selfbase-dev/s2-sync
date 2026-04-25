package main

import (
	"bufio"
	"os"
)

// tailFile returns up to n lines from the end of path (oldest first).
// Reads the whole file — n is bounded (a few hundred from the GUI), and
// the file sink rotates at 5 MiB so this stays cheap.
func tailFile(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	ring := make([]string, 0, n)
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64*1024), 1<<20)
	for s.Scan() {
		if len(ring) == n {
			ring = append(ring[1:], s.Text())
		} else {
			ring = append(ring, s.Text())
		}
	}
	return ring, s.Err()
}
