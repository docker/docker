package loggerutils

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/pkg/tracing"
	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/logger"
	"github.com/moby/moby/v2/pkg/pools"
	"go.opentelemetry.io/otel/attribute"
)

// rotateFileMetadata is a metadata of the gzip header of the compressed log file
type rotateFileMetadata struct {
	LastTime time.Time `json:"lastTime"`
}

// LogFile is Logger implementation for default Docker logging.
type LogFile struct {
	mu       sync.Mutex // protects the logfile access
	closed   chan struct{}
	rotateMu sync.Mutex // blocks the next rotation until the current rotation is completed
	// Lock out readers while performing a non-atomic sequence of filesystem
	// operations (RLock: open, Lock: rename, delete).
	//
	// fsopMu should be locked for writing only while holding rotateMu.
	fsopMu sync.RWMutex

	// Logger configuration

	capacity int64 // maximum size of each file
	maxFiles int   // maximum number of files
	compress bool  // whether old versions of log files are compressed
	perms    os.FileMode

	// Log file codec

	createDecoder MakeDecoderFn
	getTailReader GetTailReaderFunc

	// Log reader state in a 1-buffered channel.
	//
	// Share memory by communicating: receive to acquire, send to release.
	// The state struct is passed around by value so that use-after-send
	// bugs cannot escalate to data races.
	//
	// A method which receives the state value takes ownership of it. The
	// owner is responsible for either passing ownership along or sending
	// the state back to the channel. By convention, the semantics of
	// passing along ownership is expressed with function argument types.
	// Methods which take a pointer *logReadState argument borrow the state,
	// analogous to functions which require a lock to be held when calling.
	// The caller retains ownership. Calling a method which takes a
	// value logFileState argument gives ownership to the callee.
	read chan logReadState

	decompress *sharedTempFileConverter

	pos           logPos    // Current log file write position.
	f             *os.File  // Current log file for writing.
	lastTimestamp time.Time // timestamp of the last log
}

type logPos struct {
	// Size of the current file.
	size int64
	// File rotation sequence number (modulo 2**16).
	rotation uint16
}

type logReadState struct {
	// Current log file position.
	pos logPos
	// Wait list to be notified of the value of pos next time it changes.
	wait []chan<- logPos
}

// MakeDecoderFn creates a decoder
type MakeDecoderFn func(rdr io.Reader) Decoder

// Decoder is for reading logs
// It is created by the log reader by calling the `MakeDecoderFunc`
type Decoder interface {
	// Reset resets the decoder
	// Reset is called for certain events, such as log rotations
	Reset(io.Reader)
	// Decode decodes the next log message from the stream
	Decode() (*logger.Message, error)
	// Close signals to the decoder that it can release whatever resources it was using.
	Close()
}

// SizeReaderAt defines a ReaderAt that also reports its size.
// This is used for tailing log files.
type SizeReaderAt interface {
	io.Reader
	io.ReaderAt
	Size() int64
}

// GetTailReaderFunc is used to truncate a reader to only read as much as is required
// in order to get the passed in number of log lines.
// It returns the sectioned reader, the number of lines that the section reader
// contains, and any error that occurs.
type GetTailReaderFunc func(ctx context.Context, f SizeReaderAt, nLogLines int) (rdr SizeReaderAt, nLines int, err error)

// NewLogFile creates new LogFile
func NewLogFile(logPath string, capacity int64, maxFiles int, compress bool, decodeFunc MakeDecoderFn, perms os.FileMode, getTailReader GetTailReaderFunc) (*LogFile, error) {
	logFile, err := openFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, perms)
	if err != nil {
		return nil, err
	}

	size, err := logFile.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}

	pos := logPos{
		size: size,
		// Force a wraparound on first rotation to shake out any
		// modular-arithmetic bugs.
		rotation: math.MaxUint16,
	}
	st := make(chan logReadState, 1)
	st <- logReadState{pos: pos}

	return &LogFile{
		f:             logFile,
		read:          st,
		pos:           pos,
		closed:        make(chan struct{}),
		capacity:      capacity,
		maxFiles:      maxFiles,
		compress:      compress,
		decompress:    newSharedTempFileConverter(decompress),
		createDecoder: decodeFunc,
		perms:         perms,
		getTailReader: getTailReader,
	}, nil
}

// WriteLogEntry writes the provided log message to the current log file.
// This may trigger a rotation event if the max file/capacity limits are hit.
func (w *LogFile) WriteLogEntry(timestamp time.Time, marshalled []byte) error {
	select {
	case <-w.closed:
		return errors.New("cannot write because the output file was closed")
	default:
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	// Are we due for a rotation?
	if w.capacity != -1 && w.pos.size >= w.capacity {
		if err := w.rotate(); err != nil {
			return fmt.Errorf("error rotating log file: %w", err)
		}
	}

	n, err := w.f.Write(marshalled)
	if err != nil {
		return fmt.Errorf("error writing log entry: %w", err)
	}
	w.pos.size += int64(n)
	w.lastTimestamp = timestamp

	// Notify any waiting readers that there is a new log entry to read.
	st := <-w.read
	defer func() { w.read <- st }()
	st.pos = w.pos

	for _, c := range st.wait {
		c <- st.pos
	}
	// Optimization: retain the backing array to save a heap allocation next
	// time a reader appends to the list.
	if st.wait != nil {
		st.wait = st.wait[:0]
	}
	return nil
}

func (w *LogFile) rotate() (retErr error) {
	w.rotateMu.Lock()
	noCompress := w.maxFiles <= 1 || !w.compress
	defer func() {
		// If we aren't going to run the goroutine to compress the log file, then we need to unlock in this function.
		// Otherwise the lock will be released in the goroutine that handles compression.
		if retErr != nil || noCompress {
			w.rotateMu.Unlock()
		}
	}()

	fname := w.f.Name()
	if err := w.f.Close(); err != nil {
		// if there was an error during a prior rotate, the file could already be closed
		if !errors.Is(err, fs.ErrClosed) {
			return fmt.Errorf("error closing file: %w", err)
		}
	}

	file, err := func() (*os.File, error) {
		w.fsopMu.Lock()
		defer w.fsopMu.Unlock()

		if err := rotate(fname, w.maxFiles, w.compress); err != nil {
			log.G(context.TODO()).WithError(err).Warn("Error rotating log file, log data may have been lost")
		} else {
			// We may have readers working their way through the
			// current log file so we can't truncate it. We need to
			// start writing new logs to an empty file with the same
			// name as the current one so we need to rotate the
			// current file out of the way.
			if w.maxFiles < 2 {
				if err := unlink(fname); err != nil && !errors.Is(err, fs.ErrNotExist) {
					log.G(context.TODO()).WithError(err).Error("Error unlinking current log file")
				}
			} else {
				if err := os.Rename(fname, fname+".1"); err != nil && !errors.Is(err, fs.ErrNotExist) {
					log.G(context.TODO()).WithError(err).Error("Error renaming current log file")
				}
			}
		}

		// Notwithstanding the above, open with the truncate flag anyway
		// in case rotation didn't work out as planned.
		return openFile(fname, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, w.perms)
	}()
	if err != nil {
		return err
	}
	w.f = file
	w.pos = logPos{rotation: w.pos.rotation + 1}

	if noCompress {
		return nil
	}

	ts := w.lastTimestamp
	go func() {
		defer w.rotateMu.Unlock()
		// No need to hold fsopMu as at no point will the filesystem be
		// in a state which would cause problems for readers. Opening
		// the uncompressed file is tried first, falling back to the
		// compressed one. compressFile only deletes the uncompressed
		// file once the compressed one is fully written out, so at no
		// point during the compression process will a reader fail to
		// open a complete copy of the file.
		if err := compressFile(fname+".1", ts); err != nil {
			log.G(context.TODO()).WithError(err).Error("Error compressing log file after rotation")
		}
	}()

	return nil
}

func rotate(name string, maxFiles int, compress bool) error {
	if maxFiles < 2 {
		return nil
	}

	var extension string
	if compress {
		extension = ".gz"
	}

	lastFile := fmt.Sprintf("%s.%d%s", name, maxFiles-1, extension)
	err := unlink(lastFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("error removing oldest log file: %w", err)
	}

	for i := maxFiles - 1; i > 1; i-- {
		toPath := name + "." + strconv.Itoa(i) + extension
		fromPath := name + "." + strconv.Itoa(i-1) + extension
		err := os.Rename(fromPath, toPath)
		log.G(context.TODO()).WithError(err).WithField("source", fromPath).WithField("target", toPath).Trace("Rotating log file")
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}

	return nil
}

func compressFile(fileName string, lastTimestamp time.Time) (retErr error) {
	file, err := open(fileName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.G(context.TODO()).WithField("file", fileName).WithError(err).Debug("Could not open log file to compress")
			return nil
		}
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer func() {
		file.Close()
		if retErr == nil {
			err := unlink(fileName)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				retErr = fmt.Errorf("failed to remove source log file: %w", err)
			}
		}
	}()

	outFile, err := openFile(fileName+".gz", os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o640)
	if err != nil {
		return fmt.Errorf("failed to open or create gzip log file: %w", err)
	}
	defer func() {
		outFile.Close()
		if retErr != nil {
			if err := unlink(fileName + ".gz"); err != nil && !errors.Is(err, fs.ErrNotExist) {
				log.G(context.TODO()).WithError(err).Error("Error cleaning up after failed log compression")
			}
		}
	}()

	compressWriter := gzip.NewWriter(outFile)
	defer compressWriter.Close()

	// Add the last log entry timestamp to the gzip header
	extra := rotateFileMetadata{}
	extra.LastTime = lastTimestamp
	compressWriter.Header.Extra, err = json.Marshal(&extra)
	if err != nil {
		// Here log the error only and don't return since this is just an optimization.
		log.G(context.TODO()).Warningf("Failed to marshal gzip header as JSON: %v", err)
	}

	_, err = pools.Copy(compressWriter, file)
	if err != nil {
		return fmt.Errorf("error compressing log file %s: %w", fileName, err)
	}

	return nil
}

// MaxFiles return maximum number of files
func (w *LogFile) MaxFiles() int {
	return w.maxFiles
}

// Close closes underlying file and signals all readers to stop.
func (w *LogFile) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case <-w.closed:
		return nil
	default:
	}
	if err := w.f.Close(); err != nil && !errors.Is(err, fs.ErrClosed) {
		return err
	}
	close(w.closed)
	// Wait until any in-progress rotation is complete.
	w.rotateMu.Lock()
	defer w.rotateMu.Unlock()
	return nil
}

// ReadLogs decodes entries from log files.
//
// It is the caller's responsibility to call ConsumerGone on the LogWatcher.
func (w *LogFile) ReadLogs(ctx context.Context, config logger.ReadConfig) *logger.LogWatcher {
	ctx, span := tracing.StartSpan(ctx, "logger.LogFile.ReadLogs")
	defer span.End()

	span.SetAttributes(tracing.Attribute("config", config))

	watcher := logger.NewLogWatcher()
	// Lock out filesystem operations so that we can capture the read
	// position and atomically open the corresponding log file, without the
	// file getting rotated out from under us.
	w.fsopMu.RLock()
	// Capture the read position synchronously to ensure that we start
	// following from the last entry logged before ReadLogs was called,
	// which is required for flake-free unit testing.
	st := <-w.read
	pos := st.pos
	w.read <- st
	go w.readLogsLocked(ctx, pos, config, watcher)
	return watcher
}

// tailFiles must be called with w.fsopMu locked for reads.
// w.fsopMu.RUnlock() is called before returning.
func (w *LogFile) tailFiles(ctx context.Context, config logger.ReadConfig, watcher *logger.LogWatcher, current SizeReaderAt, dec Decoder, fwd *forwarder) (cont bool) {
	if config.Tail == 0 {
		w.fsopMu.RUnlock()
		return true
	}

	ctx, span := tracing.StartSpan(ctx, "logger.Logfile.TailLogs")
	defer func() {
		span.SetAttributes(attribute.Bool("continue", cont))
		span.End()
	}()

	files, err := w.openRotatedFiles(ctx, config)
	w.fsopMu.RUnlock()

	if err != nil {
		// TODO: Should we allow this to continue (as in set `cont=true`) and not error out the log stream?
		err = fmt.Errorf("error opening rotated log files: %w", err)
		span.SetStatus(err)
		watcher.Err <- err
		return false
	}

	if current.Size() > 0 {
		files = append(files, &sizeReaderAtOpener{current, "current"})
	}

	return tailFiles(ctx, files, watcher, dec, w.getTailReader, config.Tail, fwd)
}

type sizeReaderAtOpener struct {
	SizeReaderAt
	ref string
}

func (o *sizeReaderAtOpener) ReaderAt(context.Context) (sizeReaderAtCloser, error) {
	return &sizeReaderAtWithCloser{o, nil}, nil
}

func (o *sizeReaderAtOpener) Close() {}

func (o *sizeReaderAtOpener) Ref() string {
	return o.ref
}

type sizeReaderAtWithCloser struct {
	SizeReaderAt
	close func() error
}

func (r *sizeReaderAtWithCloser) ReadAt(p []byte, offset int64) (int, error) {
	if r.SizeReaderAt == nil {
		return 0, io.EOF
	}
	return r.SizeReaderAt.ReadAt(p, offset)
}

func (r *sizeReaderAtWithCloser) Read(p []byte) (int, error) {
	if r.SizeReaderAt == nil {
		return 0, io.EOF
	}
	return r.SizeReaderAt.Read(p)
}

func (r *sizeReaderAtWithCloser) Size() int64 {
	if r.SizeReaderAt == nil {
		return 0
	}
	return r.SizeReaderAt.Size()
}

func (r *sizeReaderAtWithCloser) Close() error {
	if r.close != nil {
		return r.close()
	}
	return nil
}

// readLogsLocked is the bulk of the implementation of ReadLogs.
//
// w.fsopMu must be locked for reading when calling this method.
// w.fsopMu.RUnlock() is called before returning.
func (w *LogFile) readLogsLocked(ctx context.Context, currentPos logPos, config logger.ReadConfig, watcher *logger.LogWatcher) {
	ctx, span := tracing.StartSpan(ctx, "logger.Logfile.ReadLogsLocked")
	defer span.End()

	defer close(watcher.Msg)

	currentFile, err := open(w.f.Name())
	if err != nil {
		w.fsopMu.RUnlock()
		span.SetStatus(err)
		watcher.Err <- err
		return
	}
	defer currentFile.Close()

	dec := w.createDecoder(nil)
	defer dec.Close()

	fwd := newForwarder(config)

	// At this point, w.tailFiles is responsible for unlocking w.fsopmu
	ok := w.tailFiles(ctx, config, watcher, io.NewSectionReader(currentFile, 0, currentPos.size), dec, fwd)

	if !ok {
		return
	}

	if !config.Follow {
		return
	}

	(&follow{
		LogFile:   w,
		Watcher:   watcher,
		Decoder:   dec,
		Forwarder: fwd,
	}).Do(ctx, currentFile, currentPos)
}

type fileOpener interface {
	ReaderAt(context.Context) (ra sizeReaderAtCloser, err error)
	Close()
	Ref() string
}

// simpleFileOpener just holds a reference to an already open file
type simpleFileOpener struct {
	f      *os.File
	sz     int64
	closed bool
}

func (o *simpleFileOpener) ReaderAt(context.Context) (sizeReaderAtCloser, error) {
	if o.closed {
		return nil, errors.New("file is closed")
	}

	if o.sz == 0 {
		stat, err := o.f.Stat()
		if err != nil {
			return nil, fmt.Errorf("error stating file: %w", err)
		}
		o.sz = stat.Size()
	}
	return &sizeReaderAtWithCloser{io.NewSectionReader(o.f, 0, o.sz), nil}, nil
}

func (o *simpleFileOpener) Ref() string {
	return o.f.Name()
}

func (o *simpleFileOpener) Close() {
	_ = o.f.Close()
	o.closed = true
}

// converter function used by shareTempFileConverter
func decompress(dst io.WriteSeeker, src io.ReadSeeker) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	rc, err := gzip.NewReader(src)
	if err != nil {
		return err
	}
	_, err = pools.Copy(dst, rc)
	if err != nil {
		return err
	}
	return rc.Close()
}

// compressedFileOpener holds a reference to compressed a log file and will
// lazily open a decompressed version of the file.
type compressedFileOpener struct {
	closed bool

	f *os.File

	lf       *LogFile
	ifBefore time.Time
}

func (cfo *compressedFileOpener) ReaderAt(ctx context.Context) (_ sizeReaderAtCloser, retErr error) {
	_, span := tracing.StartSpan(ctx, "logger.Logfile.Compressed.ReaderAt")
	defer func() {
		if retErr != nil {
			span.SetStatus(retErr)
		}
		span.End()
	}()

	span.SetAttributes(attribute.String("file", cfo.f.Name()))

	if cfo.closed {
		return nil, errors.New("compressed file closed")
	}

	gzr, err := gzip.NewReader(cfo.f)
	if err != nil {
		return nil, err
	}
	defer gzr.Close()

	// Extract the last log entry timestamp from the gzip header
	// Use this to determine if we even need to read this file based on inputs
	extra := &rotateFileMetadata{}
	err = json.Unmarshal(gzr.Header.Extra, extra)
	if err == nil && !extra.LastTime.IsZero() && extra.LastTime.Before(cfo.ifBefore) {
		span.SetAttributes(attribute.Bool("skip", true))
		return &sizeReaderAtWithCloser{}, nil
	}
	if err == nil {
		span.SetAttributes(attribute.Stringer("lastLogTime", extra.LastTime))
	}

	span.AddEvent("Start decompress")
	return cfo.lf.decompress.Do(cfo.f)
}

func (cfo *compressedFileOpener) Close() {
	cfo.closed = true
	cfo.f.Close()
}

func (cfo *compressedFileOpener) Ref() string {
	return cfo.f.Name()
}

type emptyFileOpener struct{}

func (emptyFileOpener) ReaderAt(context.Context) (sizeReaderAtCloser, error) {
	return &sizeReaderAtWithCloser{}, nil
}

func (emptyFileOpener) Close() {}

func (emptyFileOpener) Ref() string {
	return "null"
}

// openRotatedFiles returns a slice of files open for reading, in order from
// oldest to newest, and calls w.fsopMu.RUnlock() before returning.
//
// This method must only be called with w.fsopMu locked for reading.
func (w *LogFile) openRotatedFiles(ctx context.Context, config logger.ReadConfig) (_ []fileOpener, retErr error) {
	var out []fileOpener

	defer func() {
		if retErr != nil {
			for _, fo := range out {
				fo.Close()
			}
		}
	}()

	for i := w.maxFiles; i > 1; i-- {
		fo, err := w.openRotatedFile(ctx, i-1, config)
		if err != nil {
			return nil, err
		}
		out = append(out, fo)
	}

	return out, nil
}

func (w *LogFile) openRotatedFile(ctx context.Context, i int, config logger.ReadConfig) (fileOpener, error) {
	f, err := open(fmt.Sprintf("%s.%d", w.f.Name(), i))
	if err == nil {
		return &simpleFileOpener{
			f: f,
		}, nil
	}

	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("error opening rotated log file: %w", err)
	}

	f, err = open(fmt.Sprintf("%s.%d.gz", w.f.Name(), i))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("error opening file for decompression: %w", err)
		}
		return &emptyFileOpener{}, nil
	}

	return &compressedFileOpener{
		f:        f,
		lf:       w,
		ifBefore: config.Since,
	}, nil
}

// This is used to improve type safety around tailing logs
// Some log readers require the log file to be closed, so this makes sure all
// implementers have a closer even if it may be a no-op.
// This is opposed to asserting a type.
type sizeReaderAtCloser interface {
	SizeReaderAt
	io.Closer
}

func getTailFiles(ctx context.Context, files []fileOpener, nLines int, getTailReader GetTailReaderFunc) (_ []sizeReaderAtCloser, retErr error) {
	ctx, span := tracing.StartSpan(ctx, "logger.Logfile.CollectTailFiles")
	span.SetAttributes(attribute.Int("requested_lines", nLines))

	defer func() {
		if retErr != nil {
			span.SetStatus(retErr)
		}
		span.End()
	}()
	out := make([]sizeReaderAtCloser, 0, len(files))

	defer func() {
		if retErr != nil {
			for _, ra := range out {
				if err := ra.Close(); err != nil {
					log.G(ctx).WithError(err).Warn("Error closing log reader")
				}
			}
		}
	}()

	if nLines <= 0 {
		for _, fo := range files {
			span.AddEvent("Open file", attribute.String("file", fo.Ref()))

			ra, err := fo.ReaderAt(ctx)
			if err != nil {
				return nil, err
			}
			out = append(out, ra)

		}
		return out, nil
	}

	for i := len(files) - 1; i >= 0 && nLines > 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("stopping parsing files to tail due to error: %w", err)
		}

		fo := files[i]

		fileAttr := attribute.String("file", fo.Ref())
		span.AddEvent("Open file", fileAttr)

		ra, err := fo.ReaderAt(ctx)
		if err != nil {
			return nil, err
		}

		span.AddEvent("Scan file to tail", fileAttr, attribute.Int("remaining_lines", nLines))

		tail, n, err := getTailReader(ctx, ra, nLines)
		if err != nil {
			ra.Close()
			log.G(ctx).WithError(err).Warn("Error scanning log file for tail file request, skipping")
			continue
		}
		nLines -= n
		out = append(out, &sizeReaderAtWithCloser{tail, ra.Close})
	}

	slices.Reverse(out)

	return out, nil
}

func tailFiles(ctx context.Context, files []fileOpener, watcher *logger.LogWatcher, dec Decoder, getTailReader GetTailReaderFunc, nLines int, fwd *forwarder) (cont bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		select {
		case <-ctx.Done():
		case <-watcher.WatchConsumerGone():
			cancel()
		}
	}()

	readers, err := getTailFiles(ctx, files, nLines, getTailReader)
	if err != nil {
		watcher.Err <- err
		return false
	}

	var idx int
	defer func() {
		// Make sure all are released if there is an early return.
		if !cont {
			for _, r := range readers[idx:] {
				if err := r.Close(); err != nil {
					log.G(ctx).WithError(err).Debug("Error closing log reader")
				}
			}
		}
	}()

	for _, ra := range readers {
		select {
		case <-watcher.WatchConsumerGone():
			return false
		case <-ctx.Done():
			return false
		default:
		}

		dec.Reset(ra)

		cancel := context.AfterFunc(ctx, func() {
			if err := ra.Close(); err != nil {
				log.G(ctx).WithError(err).Debug("Error closing log reader")
			}
		})

		ok := fwd.Do(ctx, watcher, func() (*logger.Message, error) {
			msg, err := dec.Decode()
			if err != nil && !errors.Is(err, io.EOF) {
				// We have an error decoding the stream, but we don't want to error out
				// the whole log reader.
				// If we return anything other than EOF then the forwarder will return
				// false and we'll exit the loop.
				// Instead just log the error here and return an EOF so we can move to
				// the next file.
				log.G(ctx).WithError(err).Warn("Error decoding log file")
				return nil, io.EOF
			}
			return msg, err
		})
		cancel()
		idx++
		if !ok {
			return false
		}
	}

	return true
}

type forwarder struct {
	since, until time.Time
}

func newForwarder(config logger.ReadConfig) *forwarder {
	return &forwarder{since: config.Since, until: config.Until}
}

// Do reads log messages from dec and sends the messages matching the filter
// conditions to watcher. Do returns cont=true iff it has read all messages from
// dec without encountering a message with a timestamp which is after the
// configured until time.
func (fwd *forwarder) Do(ctx context.Context, watcher *logger.LogWatcher, next func() (*logger.Message, error)) (cont bool) {
	ctx, span := tracing.StartSpan(ctx, "logger.Logfile.Forward")
	defer func() {
		span.SetAttributes(attribute.Bool("continue", cont))
		span.End()
	}()

	for {
		select {
		case <-watcher.WatchConsumerGone():
			span.AddEvent("watch consumer gone")
			return false
		case <-ctx.Done():
			span.AddEvent(ctx.Err().Error())
			return false
		default:
		}

		msg, err := next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				span.AddEvent("EOF")
				return true
			}
			span.SetStatus(err)
			log.G(ctx).WithError(err).Debug("Error while decoding log entry, not continuing")
			return false
		}

		if !fwd.since.IsZero() {
			if msg.Timestamp.Before(fwd.since) {
				continue
			}
			// We've found our first message with a timestamp >= since. As message
			// timestamps might not be monotonic, we need to skip the since check for all
			// subsequent messages so we do not filter out later messages which happen to
			// have timestamps before since.
			fwd.since = time.Time{}
		}
		if !fwd.until.IsZero() && msg.Timestamp.After(fwd.until) {
			log.G(ctx).Debug("Log is newer than requested window, skipping remaining logs")
			return false
		}

		select {
		case <-ctx.Done():
			span.AddEvent(ctx.Err().Error())
			return false
		case <-watcher.WatchConsumerGone():
			span.AddEvent("watch consumer gone")
			return false
		case watcher.Msg <- msg:
		}
	}
}
