package net

import (
	"io"
	"os"
	"sync"
	"time"

	"github.com/cloudfoundry/cli/cf/formatters"
	"github.com/cloudfoundry/cli/cf/terminal"
)

//go:generate counterfeiter . ReadSeekCloser

type ReadSeekCloser interface {
	io.ReadCloser
	io.Seeker
}

type ProgressReader struct {
	r              ReadSeekCloser
	bytesRead      int64
	total          int64
	quit           chan bool
	ui             terminal.UI
	outputInterval time.Duration
	mutex          sync.RWMutex
}

func NewProgressReader(r ReadSeekCloser, ui terminal.UI, outputInterval time.Duration) *ProgressReader {
	return &ProgressReader{
		r:              r,
		ui:             ui,
		outputInterval: outputInterval,
		mutex:          sync.RWMutex{},
	}
}

// Read will read from the underlying Reader,
// each time comparing the total number of bytes read so far
// with the expected total (set by SetTotalSize)
//
// The first time Read is called, it starts up a goroutine
// which periodically prints the Reader's progress.
func (pr *ProgressReader) Read(p []byte) (int, error) {
	if pr.r == nil {
		return 0, os.ErrInvalid
	}

	n, err := pr.r.Read(p)

	if pr.total > int64(0) {
		if n > 0 {
			// Lazily create the quit channel only once.
			// This signals whether we have started the "printing" goroutine already.
			// We only want to spin up the printing goroutine the *first* time someone
			// calls Read.
			if pr.quit == nil {
				pr.quit = make(chan bool)
				go pr.printProgress(pr.quit)
			}

			pr.mutex.Lock()
			pr.bytesRead += int64(n)
			pr.mutex.Unlock()

			// Once we have read bytes = the total size we set via SetTotalSize
			// we can stop printing
			if pr.total <= pr.bytesRead {
				pr.quit <- true
				return n, err
			}
		}
	}

	return n, err
}

// Seek seeks via the underlying Seeker
//
// It then updates its running total of the number of bytes read
// Because according to the definition of the Seek interface,
// "Seek returns the new offset relative to the start of the file and an error, if any."
//
func (pr *ProgressReader) Seek(offset int64, whence int) (int64, error) {
	n, err := pr.r.Seek(offset, whence)
	pr.mutex.Lock()
	pr.bytesRead = int64(n)
	pr.mutex.Unlock()

	return n, err
}

// Close will close the underlying Closer,
// and if there is a printing goroutine running,
// signal it to quit
func (pr *ProgressReader) Close() error {
	if pr.quit != nil {
		pr.quit <- true
	}
	return pr.r.Close()
}

func (pr *ProgressReader) printProgress(quit chan bool) {
	timer := time.NewTicker(pr.outputInterval)

	for {
		select {
		case <-quit:
			//The spaces are there to ensure we overwrite the entire line
			//before using the terminal printer to output Done Uploading
			pr.ui.PrintCapturingNoOutput("\r                             ")
			pr.ui.Say("\rDone uploading")
			return
		case <-timer.C:
			pr.mutex.RLock()
			pr.ui.PrintCapturingNoOutput("\r%s uploaded...", formatters.ByteSize(pr.bytesRead))
			pr.mutex.RUnlock()
		}
	}
}

func (pr *ProgressReader) SetTotalSize(size int64) {
	pr.total = size
}
