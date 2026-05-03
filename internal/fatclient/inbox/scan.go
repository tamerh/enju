package inbox

// tailJSONL reads a JSONL file backward from EOF in 64KB chunks
// and invokes fn once per line, newest-first. fn returns true to
// stop scanning early. Missing file → no-op (nil error).
//
// Backward scan keeps inbox latency bounded on large logs:
// "latest 200 ready events" against a 100MB log only reads the
// tail, not the whole file.

import (
	"bytes"
	"os"
)

func tailJSONL(path string, fn func(line []byte) (stop bool)) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return err
	}
	fileSize := stat.Size()
	if fileSize == 0 {
		return nil
	}

	const chunkSize = int64(64 * 1024)

	pos := fileSize
	var carry []byte
	for pos > 0 {
		readSize := chunkSize
		if readSize > pos {
			readSize = pos
		}
		pos -= readSize

		buf := make([]byte, readSize)
		if _, err := f.ReadAt(buf, pos); err != nil {
			return err
		}
		if len(carry) > 0 {
			buf = append(buf, carry...)
		}

		end := len(buf)
		stop := false
		for i := len(buf) - 1; i >= 0; i-- {
			if buf[i] != '\n' {
				continue
			}
			line := bytes.TrimSpace(buf[i+1 : end])
			end = i
			if len(line) == 0 {
				continue
			}
			if fn(line) {
				stop = true
				break
			}
		}
		if stop {
			return nil
		}

		if pos == 0 {
			line := bytes.TrimSpace(buf[:end])
			if len(line) > 0 {
				fn(line)
			}
			return nil
		}
		carry = make([]byte, end)
		copy(carry, buf[:end])
	}
	return nil
}
