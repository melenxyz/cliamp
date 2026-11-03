package soundcloudx

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"
)

// tailPoll is how often a tailReader checks a file that has not grown.
var tailPoll = 50 * time.Millisecond

// errTailClosed is returned by a Read on a closed tailReader.
var errTailClosed = errors.New("tail reader closed")

// tailReader reads a file that another process is still writing. At the end
// of the file it waits for more data until done is closed, then drains what
// is left and returns io.EOF. yt-dlp only appends whole HLS fragments to its
// .part file, so the reader sees one contiguous fragmented-MP4 stream.
//
// It keeps the file open, so the data stays readable after yt-dlp renames
// the .part file or replaces the finished file during its fixup step.
type tailReader struct {
	f       *os.File
	done    <-chan struct{}
	closed  chan struct{}
	closeMu sync.Once
}

// openTail opens path, waiting for it to appear. It returns fs.ErrNotExist
// when done closes before the file exists, and errTailClosed when stop
// closes first.
func openTail(path string, done, stop <-chan struct{}) (*tailReader, error) {
	for {
		f, err := os.Open(path)
		if err == nil {
			return &tailReader{f: f, done: done, closed: make(chan struct{})}, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		select {
		case <-done:
			return nil, fs.ErrNotExist
		case <-stop:
			return nil, errTailClosed
		case <-time.After(tailPoll):
		}
	}
}

func (t *tailReader) Read(p []byte) (int, error) {
	for {
		n, err := t.f.Read(p)
		if n > 0 {
			return n, nil
		}
		if err != nil && err != io.EOF {
			return 0, err
		}
		select {
		case <-t.closed:
			return 0, errTailClosed
		case <-t.done:
			// The writer finished; whatever it wrote last is in the file now.
			n, err := t.f.Read(p)
			if n > 0 {
				return n, nil
			}
			if err == nil {
				err = io.EOF
			}
			return 0, err
		case <-time.After(tailPoll):
		}
	}
}

// Close unblocks a waiting Read and closes the file.
func (t *tailReader) Close() error {
	var err error
	t.closeMu.Do(func() {
		close(t.closed)
		err = t.f.Close()
	})
	return err
}
