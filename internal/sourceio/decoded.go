package sourceio

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sync"

	sessionio "github.com/nikitatsym/agent-session-io"
)

type decodedRecordIndex struct {
	record uint64
	line   uint64
	digest [sha256.Size]byte
}

// DecodedJSONLGeneration verifies a final decoded stream against a bounded
// physical compressed container in a second streaming pass.
type DecodedJSONLGeneration struct {
	mu sync.Mutex

	spec        DecodedFileSpec
	policy      RecordSizePolicy
	identity    os.FileInfo
	size        int64
	modTime     int64
	revision    sessionio.Revision
	records     []decodedRecordIndex
	nextIndex   int
	pass        *decodedPass
	closed      bool
	exhausted   bool
	terminalErr error
	closeResult error
}

type decodedPass struct {
	file      *os.File
	physical  *boundedPhysicalReader
	decoder   io.ReadCloser
	reader    *bufio.Reader
	candidate []byte
	policy    RecordSizePolicy
	codec     string
}

type boundedPhysicalReader struct {
	ctx       context.Context
	reader    io.Reader
	remaining int64
	hash      hash.Hash
	read      int64
}

func (reader *boundedPhysicalReader) setContext(ctx context.Context) {
	reader.ctx = ctx
}

func openDecodedJSONLGeneration(
	ctx context.Context,
	spec DecodedFileSpec,
	options DecodedOpenOptions,
) (*DecodedJSONLGeneration, error) {
	if err := validateDecodedOpenRequest(ctx, spec, options); err != nil {
		return nil, err
	}
	file, err := os.Open(spec.OpenPath)
	if err != nil {
		return nil, fmt.Errorf("sourceio: open decoded container %q: %w", spec.OpenPath, err)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, closeAfterError(file, fmt.Errorf("sourceio: stat decoded container %q: %w", spec.OpenPath, err))
	}
	if !info.Mode().IsRegular() {
		return nil, closeAfterError(file, fmt.Errorf("sourceio: decoded container %q is not a regular file", spec.OpenPath))
	}
	physical := newBoundedPhysicalReader(ctx, file, info.Size())
	decoder, err := spec.OpenDecoder(physical)
	if err != nil {
		return nil, closeAfterError(file, fmt.Errorf("sourceio: open %s decoder for %q: %w", spec.Codec, spec.OpenPath, err))
	}
	records, scanErr := scanDecodedJSONL(ctx, bufio.NewReader(decoder), spec, options)
	closeErr := decoder.Close()
	if scanErr == nil && closeErr != nil {
		scanErr = fmt.Errorf("sourceio: close %s decoder for %q: %w", spec.Codec, spec.OpenPath, closeErr)
	}
	if scanErr == nil {
		scanErr = physical.requireConsumed()
	}
	if scanErr == nil {
		scanErr = verifyDecodedContainer(file, spec.OpenPath, info)
	}
	if scanErr != nil {
		return nil, closeAfterError(file, scanErr)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("sourceio: close decoded container %q: %w", spec.OpenPath, err)
	}
	return &DecodedJSONLGeneration{
		spec:     spec,
		policy:   options.SizePolicy,
		identity: info,
		size:     info.Size(),
		modTime:  info.ModTime().UnixNano(),
		revision: revisionFromDigest(hashDigest(physical.hash)),
		records:  records,
	}, nil
}

func validateDecodedOpenRequest(ctx context.Context, spec DecodedFileSpec, options DecodedOpenOptions) error {
	if ctx == nil {
		return errors.New("sourceio: context must not be nil")
	}
	if spec.OpenPath == "" || spec.Locator.Root == "" || spec.Locator.Path == "" {
		return errors.New("sourceio: decoded container path and base locator must not be empty")
	}
	if spec.Locator.Record != nil || spec.Locator.Line != nil || spec.Locator.ByteRange != nil {
		return errors.New("sourceio: decoded base file locator must not identify a record")
	}
	if spec.Codec == "" || spec.OpenDecoder == nil {
		return errors.New("sourceio: decoded container codec and decoder must not be empty")
	}
	if options.SizePolicy.MaxBytes != UnlimitedRecordBytes && options.SizePolicy.MaxBytes <= 0 {
		return fmt.Errorf("sourceio: record size limit must be positive or %d", UnlimitedRecordBytes)
	}
	return nil
}

func newBoundedPhysicalReader(ctx context.Context, reader io.Reader, size int64) *boundedPhysicalReader {
	return &boundedPhysicalReader{ctx: ctx, reader: reader, remaining: size, hash: sha256.New()}
}

func (reader *boundedPhysicalReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(data)) > reader.remaining {
		data = data[:reader.remaining]
	}
	count, err := reader.reader.Read(data)
	if count > 0 {
		_, _ = reader.hash.Write(data[:count])
		reader.remaining -= int64(count)
		reader.read += int64(count)
	}
	if contextErr := reader.ctx.Err(); contextErr != nil {
		return count, contextErr
	}
	return count, err
}

func (reader *boundedPhysicalReader) requireConsumed() error {
	if reader.remaining == 0 {
		return nil
	}
	return fmt.Errorf("sourceio: decoder left %d physical container bytes unread", reader.remaining)
}

func scanDecodedJSONL(
	ctx context.Context,
	reader *bufio.Reader,
	spec DecodedFileSpec,
	options DecodedOpenOptions,
) ([]decodedRecordIndex, error) {
	var records []decodedRecordIndex
	var candidate []byte
	recordNumber := uint64(1)
	lineNumber := uint64(1)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fragment, readErr := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			candidate = append(candidate, fragment...)
		}
		switch {
		case readErr == nil:
			index, record, err := indexedDecodedRecord(spec, options.SizePolicy, recordNumber, lineNumber, candidate, true)
			if err != nil {
				return nil, err
			}
			records = append(records, index)
			if err := observeDecodedRecord(options.ObserveRecord, record); err != nil {
				return nil, err
			}
			candidate = nil
			recordNumber++
			lineNumber++
		case errors.Is(readErr, bufio.ErrBufferFull):
			if err := enforceDecodedRecordSize(spec, options.SizePolicy, recordNumber, lineNumber, candidate); err != nil {
				return nil, err
			}
		case errors.Is(readErr, io.EOF):
			if len(candidate) == 0 {
				return records, nil
			}
			index, record, err := indexedDecodedRecord(spec, options.SizePolicy, recordNumber, lineNumber, candidate, false)
			if err != nil {
				return nil, err
			}
			records = append(records, index)
			if err := observeDecodedRecord(options.ObserveRecord, record); err != nil {
				return nil, err
			}
			return records, nil
		default:
			return nil, fmt.Errorf("sourceio: decode %s container %q: %w", spec.Codec, spec.OpenPath, readErr)
		}
	}
}

func enforceDecodedRecordSize(spec DecodedFileSpec, policy RecordSizePolicy, record, line uint64, raw []byte) error {
	data, _ := SplitJSONLRecord(raw, false)
	if policy.MaxBytes == UnlimitedRecordBytes || int64(len(data)) <= policy.MaxBytes {
		return nil
	}
	return &RecordTooLargeError{Locator: decodedRecordLocator(spec.Locator, record, line), Limit: policy.MaxBytes, ObservedAtLeast: int64(len(data))}
}

func indexedDecodedRecord(spec DecodedFileSpec, policy RecordSizePolicy, record, line uint64, raw []byte, terminated bool) (decodedRecordIndex, DecodedJSONLRecord, error) {
	data, framing := SplitJSONLRecord(raw, terminated)
	if policy.MaxBytes != UnlimitedRecordBytes && int64(len(data)) > policy.MaxBytes {
		return decodedRecordIndex{}, DecodedJSONLRecord{}, &RecordTooLargeError{Locator: decodedRecordLocator(spec.Locator, record, line), Limit: policy.MaxBytes, ObservedAtLeast: int64(len(data))}
	}
	if err := validateJSONRecord(data); err != nil {
		return decodedRecordIndex{}, DecodedJSONLRecord{}, &MalformedJSONLError{Locator: decodedRecordLocator(spec.Locator, record, line), Err: err}
	}
	decoded := DecodedJSONLRecord{Record: record, Line: line, Data: data, Framing: framing}
	return decodedRecordIndex{record: record, line: line, digest: sha256.Sum256(raw)}, decoded, nil
}

// SplitJSONLRecord separates JSON data from exact LF or CRLF framing.
func SplitJSONLRecord(raw []byte, terminated bool) ([]byte, []byte) {
	if !terminated {
		return raw, nil
	}
	if len(raw) > 1 && raw[len(raw)-2] == '\r' {
		return raw[:len(raw)-2], raw[len(raw)-2:]
	}
	return raw[:len(raw)-1], raw[len(raw)-1:]
}

func observeDecodedRecord(observer DecodedRecordObserver, record DecodedJSONLRecord) error {
	if observer == nil {
		return nil
	}
	if err := observer(record); err != nil {
		return fmt.Errorf("sourceio: observe decoded record %d: %w", record.Record, err)
	}
	return nil
}

func decodedRecordLocator(base sessionio.FileLocator, record, line uint64) sessionio.SourceLocator {
	return (DecodedJSONLRecord{Record: record, Line: line}).SourceLocator(base)
}

func (generation *DecodedJSONLGeneration) PhysicalMetadata() (int64, int64) {
	return generation.size, generation.modTime
}

func (generation *DecodedJSONLGeneration) Revision() sessionio.Revision {
	generation.mu.Lock()
	defer generation.mu.Unlock()
	return generation.revision
}

func (generation *DecodedJSONLGeneration) Next(ctx context.Context) (DecodedJSONLRecord, error) {
	generation.mu.Lock()
	defer generation.mu.Unlock()
	if generation.closed {
		return DecodedJSONLRecord{}, sessionio.ErrStreamClosed
	}
	if generation.terminalErr != nil {
		return DecodedJSONLRecord{}, generation.terminalErr
	}
	if generation.exhausted {
		return DecodedJSONLRecord{}, io.EOF
	}
	if ctx == nil {
		return DecodedJSONLRecord{}, errors.New("sourceio: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return DecodedJSONLRecord{}, err
	}
	if generation.pass == nil {
		pass, err := generation.openSecondPass(ctx)
		if err != nil {
			generation.terminalErr = err
			return DecodedJSONLRecord{}, err
		}
		generation.pass = pass
	}
	generation.pass.physical.setContext(ctx)
	recordNumber := uint64(generation.nextIndex + 1)
	lineNumber := recordNumber
	if generation.nextIndex < len(generation.records) {
		recordNumber = generation.records[generation.nextIndex].record
		lineNumber = generation.records[generation.nextIndex].line
	}
	raw, terminated, err := generation.pass.nextRaw(ctx, generation.spec, recordNumber, lineNumber)
	if err != nil {
		return generation.fail(err)
	}
	if raw == nil {
		err := generation.finishSecondPass()
		if err == nil && generation.nextIndex != len(generation.records) {
			err = generation.changedError(generation.nextIndex)
		}
		if err != nil {
			return generation.fail(err)
		}
		generation.exhausted = true
		return DecodedJSONLRecord{}, io.EOF
	}
	if generation.nextIndex == len(generation.records) {
		return generation.fail(generation.changedError(generation.nextIndex))
	}
	expected := generation.records[generation.nextIndex]
	actual := sha256.Sum256(raw)
	if actual != expected.digest {
		return generation.fail(generation.changedError(generation.nextIndex))
	}
	_, record, err := indexedDecodedRecord(generation.spec, generation.policy, expected.record, expected.line, raw, terminated)
	if err != nil {
		return generation.fail(err)
	}
	generation.nextIndex++
	return record, nil
}

func (generation *DecodedJSONLGeneration) openSecondPass(ctx context.Context) (*decodedPass, error) {
	file, err := os.Open(generation.spec.OpenPath)
	if err != nil {
		return nil, fmt.Errorf("sourceio: reopen decoded container %q: %w", generation.spec.OpenPath, err)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, closeAfterError(file, fmt.Errorf("sourceio: stat decoded container %q: %w", generation.spec.OpenPath, err))
	}
	if !sameDecodedContainer(generation.identity, info, generation.size) {
		return nil, closeAfterError(file, generation.changedError(0))
	}
	physical := newBoundedPhysicalReader(ctx, file, generation.size)
	decoder, err := generation.spec.OpenDecoder(physical)
	if err != nil {
		return nil, closeAfterError(file, fmt.Errorf("sourceio: open %s decoder for %q: %w", generation.spec.Codec, generation.spec.OpenPath, err))
	}
	return &decodedPass{file: file, physical: physical, decoder: decoder, reader: bufio.NewReader(decoder), policy: generation.policy, codec: generation.spec.Codec}, nil
}

func (pass *decodedPass) nextRaw(ctx context.Context, spec DecodedFileSpec, record, line uint64) ([]byte, bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		fragment, readErr := pass.reader.ReadSlice('\n')
		if len(fragment) > 0 {
			pass.candidate = append(pass.candidate, fragment...)
		}
		switch {
		case readErr == nil:
			raw := pass.candidate
			pass.candidate = nil
			return raw, true, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			if err := enforceDecodedRecordSize(spec, pass.policy, record, line, pass.candidate); err != nil {
				return nil, false, err
			}
			continue
		case errors.Is(readErr, io.EOF):
			if len(pass.candidate) == 0 {
				return nil, false, nil
			}
			raw := pass.candidate
			pass.candidate = nil
			return raw, false, nil
		default:
			return nil, false, fmt.Errorf("sourceio: decode %s container: %w", pass.codec, readErr)
		}
	}
}

func (generation *DecodedJSONLGeneration) finishSecondPass() error {
	if generation.pass == nil {
		return nil
	}
	pass := generation.pass
	generation.pass = nil
	closeErr := pass.decoder.Close()
	consumeErr := pass.physical.requireConsumed()
	stat, statErr := pass.file.Stat()
	fileErr := pass.file.Close()
	if closeErr != nil {
		return closeErr
	}
	if consumeErr != nil {
		return consumeErr
	}
	if statErr != nil {
		return statErr
	}
	if fileErr != nil {
		return fileErr
	}
	actual := hashDigest(pass.physical.hash)
	if !sameDecodedContainer(generation.identity, stat, generation.size) || actual != mustRevisionDigest(generation.revision) {
		return generation.changedErrorWithActual(generation.nextIndex, actual)
	}
	return nil
}

func (generation *DecodedJSONLGeneration) closePass(err error) error {
	if generation.pass == nil {
		return err
	}
	pass := generation.pass
	generation.pass = nil
	return errors.Join(err, pass.decoder.Close(), pass.file.Close())
}

func (generation *DecodedJSONLGeneration) fail(err error) (DecodedJSONLRecord, error) {
	generation.terminalErr = generation.closePass(err)
	return DecodedJSONLRecord{}, generation.terminalErr
}

func (generation *DecodedJSONLGeneration) changedError(index int) error {
	actual := [sha256.Size]byte{}
	if generation.pass != nil {
		actual = hashDigest(generation.pass.physical.hash)
	}
	return generation.changedErrorWithActual(index, actual)
}

func (generation *DecodedJSONLGeneration) changedErrorWithActual(index int, actual [sha256.Size]byte) error {
	locator := sessionio.SourceLocator{Kind: sessionio.LocatorKindFile, File: &generation.spec.Locator}
	if index < len(generation.records) {
		locator = decodedRecordLocator(generation.spec.Locator, generation.records[index].record, generation.records[index].line)
	}
	return &ChangedSourceError{Locator: locator, ExpectedSHA256: mustRevisionDigest(generation.revision), ActualSHA256: actual}
}

func mustRevisionDigest(revision sessionio.Revision) [sha256.Size]byte {
	digest, err := revisionDigest(revision)
	if err != nil {
		panic(err)
	}
	return digest
}

func sameDecodedContainer(expected, actual os.FileInfo, size int64) bool {
	return actual != nil && actual.Mode().IsRegular() && actual.Size() == size && os.SameFile(expected, actual)
}

func verifyDecodedContainer(file *os.File, path string, expected os.FileInfo) error {
	current, err := file.Stat()
	if err != nil {
		return fmt.Errorf("sourceio: verify decoded container %q: %w", path, err)
	}
	if !sameDecodedContainer(expected, current, expected.Size()) {
		return fmt.Errorf("sourceio: decoded container %q changed while acquiring", path)
	}
	return nil
}

func (generation *DecodedJSONLGeneration) Close() error {
	generation.mu.Lock()
	defer generation.mu.Unlock()
	if generation.closed {
		return generation.closeResult
	}
	generation.closed = true
	generation.closeResult = generation.closePass(nil)
	return generation.closeResult
}
