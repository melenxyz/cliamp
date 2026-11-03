package soundcloudx

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopxl/beep/v2"
)

const (
	sampleRate = 44100
	channels   = 2
	frameBytes = 4 // s16le stereo

	// firstAudioTimeout bounds the wait for ffmpeg's first PCM bytes.
	firstAudioTimeout = 30 * time.Second
)

// ffmpegBin is the ffmpeg executable. Tests point it at a fake.
var ffmpegBin = "ffmpeg"

var pcmFormat = beep.Format{SampleRate: sampleRate, NumChannels: channels, Precision: 2}

// decodeProc is one ffmpeg process turning the track into s16le PCM.
type decodeProc struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	r      *bufio.Reader
	input  io.Closer // the tail reader feeding stdin, or nil for a file
	stderr *strings.Builder

	stopOnce sync.Once
}

// startDecode runs ffmpeg on input from offset. input is a file path, or
// "pipe:0" with stdin set. A file seeks before decoding; a pipe decodes from
// the start and drops the audio before offset, which is fast for data that
// is already on disk.
func startDecode(input string, stdin io.ReadCloser, offset time.Duration) (*decodeProc, error) {
	args := []string{"-hide_banner", "-loglevel", "error"}
	ss := strconv.FormatFloat(offset.Seconds(), 'f', 3, 64)
	if stdin == nil && offset > 0 {
		args = append(args, "-ss", ss)
	}
	args = append(args, "-i", input)
	if stdin != nil && offset > 0 {
		args = append(args, "-ss", ss)
	}
	args = append(args, "-vn", "-f", "s16le", "-acodec", "pcm_s16le",
		"-ar", strconv.Itoa(sampleRate), "-ac", strconv.Itoa(channels), "pipe:1")

	cmd := exec.Command(ffmpegBin, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		closeIf(stdin)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		closeIf(stdin)
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	d := &decodeProc{cmd: cmd, stdout: stdout, r: bufio.NewReaderSize(stdout, 64*1024), stderr: stderr}
	if stdin != nil {
		d.input = stdin
	}
	if err := d.waitFirstAudio(firstAudioTimeout); err != nil {
		d.stop()
		return nil, err
	}
	return d, nil
}

func closeIf(c io.Closer) {
	if c != nil {
		_ = c.Close()
	}
}

// waitFirstAudio waits until ffmpeg produced one PCM frame.
func (d *decodeProc) waitFirstAudio(timeout time.Duration) error {
	errc := make(chan error, 1)
	go func() {
		_, err := d.r.Peek(frameBytes)
		errc <- err
	}()
	select {
	case err := <-errc:
		if err == nil {
			return nil
		}
		d.stop()
		if msg := lastLine(d.stderr.String()); msg != "" {
			return fmt.Errorf("ffmpeg: %s", msg)
		}
		return fmt.Errorf("ffmpeg produced no audio: %w", err)
	case <-time.After(timeout):
		d.stop()
		<-errc
		return fmt.Errorf("timed out waiting for audio (%v)", timeout)
	}
}

// stop closes the input before killing ffmpeg, so Wait does not block on
// the stdin copy.
func (d *decodeProc) stop() {
	d.stopOnce.Do(func() {
		closeIf(d.input)
		_ = d.stdout.Close()
		if d.cmd.Process != nil {
			_ = d.cmd.Process.Kill()
		}
		_ = d.cmd.Wait()
	})
}

// opener starts a decode of the track at offset.
type opener func(offset time.Duration) (*decodeProc, error)

// pcmStreamer is the beep.StreamSeekCloser for one soundcloudx track. Its
// source can change between seeks: a track that was downloading when it
// started seeks in the finished file once the download is committed.
//
// Stream can block while the download catches up, holding mu. Position and
// Len read atomics so the UI never waits on it, and Close kills the running
// ffmpeg before taking mu, which ends a blocked read.
type pcmStreamer struct {
	mu   sync.Mutex
	open opener
	proc *decodeProc
	buf  []byte

	cur     atomic.Pointer[decodeProc] // proc, readable without mu
	pos     atomic.Int64               // in frames
	total   atomic.Int64               // in frames; 0 when unknown
	closing atomic.Bool

	errMu   sync.Mutex
	err     error
	errFn   func() error // reports a failed download after the audio ran out
	onClose func()
}

func newPCMStreamer(open opener, dur time.Duration, errFn func() error, onClose func()) (*pcmStreamer, error) {
	proc, err := open(0)
	if err != nil {
		return nil, err
	}
	s := &pcmStreamer{open: open, proc: proc, errFn: errFn, onClose: onClose}
	s.cur.Store(proc)
	s.total.Store(int64(dur.Seconds() * sampleRate))
	return s, nil
}

func (s *pcmStreamer) setErr(err error) {
	s.errMu.Lock()
	s.err = err
	s.errMu.Unlock()
}

func (s *pcmStreamer) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *pcmStreamer) Stream(samples [][2]float64) (int, bool) {
	if s.closing.Load() {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing.Load() || s.proc == nil || s.Err() != nil {
		return 0, false
	}
	need := len(samples) * frameBytes
	if cap(s.buf) < need {
		s.buf = make([]byte, need)
	}
	buf := s.buf[:need]
	n, err := io.ReadFull(s.proc.r, buf)
	frames := n / frameBytes
	for i := range frames {
		b := buf[i*frameBytes:]
		samples[i][0] = float64(int16(uint16(b[0])|uint16(b[1])<<8)) / 32768
		samples[i][1] = float64(int16(uint16(b[2])|uint16(b[3])<<8)) / 32768
	}
	pos := s.pos.Add(int64(frames))
	if err != nil && frames == 0 {
		if s.closing.Load() {
			return 0, false
		}
		if s.errFn != nil {
			if derr := s.errFn(); derr != nil {
				s.setErr(derr)
			}
		}
		if total := s.total.Load(); total == 0 || pos < total {
			// Learn the real length so the seek bar ends where the audio did.
			s.total.Store(pos)
		}
		return 0, false
	}
	return frames, true
}

func (s *pcmStreamer) Len() int      { return int(s.total.Load()) }
func (s *pcmStreamer) Position() int { return int(s.pos.Load()) }

// Seek restarts ffmpeg at frame p. The old decode keeps playing if the new
// one cannot start.
func (s *pcmStreamer) Seek(p int) error {
	if s.closing.Load() {
		return errors.New("soundcloudx: streamer closed")
	}
	if p < 0 {
		p = 0
	}
	if total := int(s.total.Load()); total > 0 && p > total {
		p = total
	}
	proc, err := s.open(time.Duration(float64(p) / sampleRate * float64(time.Second)))
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing.Load() {
		proc.stop()
		return errors.New("soundcloudx: streamer closed")
	}
	if s.proc != nil {
		s.proc.stop()
	}
	s.proc = proc
	s.cur.Store(proc)
	s.pos.Store(int64(p))
	s.setErr(nil)
	return nil
}

func (s *pcmStreamer) Close() error {
	if !s.closing.CompareAndSwap(false, true) {
		return nil
	}
	// End a read that waits on the download before taking mu.
	if proc := s.cur.Load(); proc != nil {
		proc.stop()
	}
	s.mu.Lock()
	s.proc = nil
	s.cur.Store(nil)
	s.mu.Unlock()
	if s.onClose != nil {
		s.onClose()
	}
	return nil
}
